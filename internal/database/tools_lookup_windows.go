//go:build windows

package database

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	machineEnvRegistryPath = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	trustedInstallerSID    = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	fileDeleteChild        = 0x0040
	windowsWriteMask       = windows.FILE_WRITE_DATA |
		windows.FILE_APPEND_DATA |
		windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES |
		windows.DELETE |
		fileDeleteChild |
		windows.WRITE_DAC |
		windows.WRITE_OWNER |
		windows.GENERIC_WRITE |
		windows.GENERIC_ALL
)

func lookupTool(name string) (string, error) {
	return lookupFromSearchPath(name, exec.LookPath, readWindowsMachinePath, isTrustedWindowsPath, findWindowsExecutable)
}

// readWindowsMachinePath reads only the machine (HKLM) PATH so a running
// privileged service can discover newly installed tools without consulting HKCU.
func readWindowsMachinePath() (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()

	raw, _, err := key.GetStringValue("Path")
	if err != nil {
		return "", err
	}

	return registry.ExpandString(raw)
}

func findWindowsExecutable(dir, name string) (string, bool) {
	candidate := name
	if !strings.HasSuffix(strings.ToLower(candidate), ".exe") {
		candidate += ".exe"
	}

	fullPath := filepath.Clean(filepath.Join(dir, candidate))
	info, err := os.Lstat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	if !isTrustedWindowsPath(fullPath) {
		return "", false
	}

	return fullPath, true
}

// isTrustedWindowsPath rejects reparse points and any path whose owner or
// write-capable ACEs include a non-privileged principal.
func isTrustedWindowsPath(path string) bool {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(utf16Path)
	if err != nil || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false
	}

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}

	trusted, err := trustedWindowsSIDs()
	if err != nil {
		return false
	}

	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !isSIDTrusted(owner, trusted) {
		return false
	}

	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return false
	}

	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Header.AceSize < uint16(unsafe.Sizeof(*ace)) {
			return false
		}
		if ace.Mask&windowsWriteMask == 0 {
			continue
		}

		// GetAce supplies an OS-validated ACE; SidStart is the beginning of its
		// variable-length SID, not a separate pointer.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !isSIDTrusted(sid, trusted) {
			return false
		}
	}

	return true
}

func trustedWindowsSIDs() ([]*windows.SID, error) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	installer, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		return nil, err
	}

	return []*windows.SID{system, admins, installer}, nil
}

func isSIDTrusted(candidate *windows.SID, trusted []*windows.SID) bool {
	for _, allowed := range trusted {
		if candidate.Equals(allowed) {
			return true
		}
	}

	return false
}
