//go:build windows

package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	wmSettingChange  = 0x001A
	smtoAbortIfHung  = 0x0002
	hwndBroadcast    = 0xFFFF
	pathNotifyTimout = 5000
)

var (
	user32DLL              = windows.NewLazySystemDLL("user32.dll")
	procSendMessageTimeout = user32DLL.NewProc("SendMessageTimeoutW")
)

func findOffPathCandidates(names []string) []candidateBinary {
	dirs := windowsStandardToolDirs()
	var results []candidateBinary

	for _, dir := range dirs {
		dirTrusted := isTrustedWindowsPath(dir)
		for _, name := range names {
			exeName := name
			if !strings.HasSuffix(strings.ToLower(exeName), ".exe") {
				exeName += ".exe"
			}
			fullPath := filepath.Clean(filepath.Join(dir, exeName))
			info, err := os.Lstat(fullPath)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}

			trusted := dirTrusted && isTrustedWindowsPath(fullPath)
			results = append(results, candidateBinary{
				name:    name,
				path:    fullPath,
				dir:     dir,
				trusted: trusted,
			})
		}
	}

	return results
}

func windowsStandardToolDirs() []string {
	var patterns []string
	for _, envName := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		root := strings.TrimSpace(os.Getenv(envName))
		if root == "" {
			continue
		}
		patterns = append(patterns,
			filepath.Join(root, "MariaDB *", "bin"),
			filepath.Join(root, "MySQL", "MySQL Server *", "bin"),
			filepath.Join(root, "MySQL", "MySQL Shell *", "bin"),
		)
	}
	// Include common FiveM Windows stack paths so setup can report them clearly
	// when their permissions prevent automatic trust.
	patterns = append(patterns,
		`C:\xampp\mysql\bin`,
		`C:\laragon\bin\mysql\*\bin`,
		`C:\wamp64\bin\mysql\*\bin`,
		`C:\wamp64\bin\mariadb\*\bin`,
	)

	seen := make(map[string]struct{})
	var dirs []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			continue
		}
		// Prefer newer versioned install folders first (reverse lexicographical).
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		for _, match := range matches {
			clean := normalizeWindowsPathDir(match)
			info, err := os.Stat(clean)
			if err != nil || !info.IsDir() {
				continue
			}
			key := strings.ToLower(clean)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			dirs = append(dirs, clean)
		}
	}

	return dirs
}

// AddToSystemPath appends a verified, trusted directory to the Windows machine
// PATH registry value without expanding existing %VAR% entries, updates the
// current process PATH, and broadcasts WM_SETTINGCHANGE.
func AddToSystemPath(dir string) error {
	clean := normalizeWindowsPathDir(dir)
	if !isLocalWindowsAbsPath(clean) {
		return errors.New("system PATH entry must be a local drive-qualified directory")
	}
	if !isTrustedWindowsPath(clean) {
		return errors.New("refusing to add an unprotected directory to System PATH; folder must be writable only by Administrators/SYSTEM")
	}

	key, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvRegistryPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("cannot open System PATH in registry (run as Administrator): %w", err)
	}
	defer key.Close()

	raw, _, err := key.GetStringValue("Path")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("cannot read System PATH from registry: %w", err)
	}

	updated, changed, err := appendWindowsPathEntry(raw, clean)
	if err != nil {
		return err
	}

	if changed {
		if err := key.SetExpandStringValue("Path", updated); err != nil {
			return fmt.Errorf("cannot write System PATH to registry: %w", err)
		}
		broadcastEnvironmentChange()
	}

	if current := os.Getenv("PATH"); !windowsPathContains(current, clean) {
		next := clean
		if trimmed := strings.TrimRight(current, "; "); trimmed != "" {
			next = trimmed + ";" + clean
		}
		_ = os.Setenv("PATH", next)
	}

	return nil
}

func windowsPathContains(raw, dir string) bool {
	target := strings.ToLower(normalizeWindowsPathDir(dir))
	for _, entry := range parseWindowsSearchPath(raw) {
		if strings.ToLower(entry) == target {
			return true
		}
	}
	return false
}

func broadcastEnvironmentChange() {
	envPtr, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	var result uintptr
	_, _, _ = procSendMessageTimeout.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(envPtr)),
		uintptr(smtoAbortIfHung),
		uintptr(pathNotifyTimout),
		uintptr(unsafe.Pointer(&result)),
	)
}
