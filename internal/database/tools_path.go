package database

import (
	"os/exec"
	"strings"
)

func lookupFromSearchPath(
	name string,
	processLookup func(string) (string, error),
	machinePath func() (string, error),
	trustedDir func(string) bool,
	findExecutable func(dir, name string) (string, bool),
) (string, error) {
	if path, err := processLookup(name); err == nil {
		return path, nil
	}

	raw, err := machinePath()
	if err != nil || strings.TrimSpace(raw) == "" {
		return "", exec.ErrNotFound
	}

	for _, dir := range parseWindowsSearchPath(raw) {
		if !trustedDir(dir) {
			continue
		}
		if candidate, ok := findExecutable(dir, name); ok {
			return candidate, nil
		}
	}

	return "", exec.ErrNotFound
}

func parseWindowsSearchPath(raw string) []string {
	entries := strings.Split(raw, ";")
	dirs := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))

	for _, entry := range entries {
		dir := strings.TrimSpace(entry)
		dir = strings.Trim(dir, "\"")
		dir = strings.TrimSpace(dir)
		if !isLocalWindowsAbsPath(dir) {
			continue
		}

		// Preserve first-seen search order while deduplicating case-insensitively.
		normalized := strings.TrimRight(strings.ReplaceAll(dir, "/", `\`), `\`)
		if len(normalized) == 2 && normalized[1] == ':' {
			normalized += `\`
		}
		key := strings.ToLower(normalized)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		dirs = append(dirs, normalized)
	}

	return dirs
}

// isLocalWindowsAbsPath accepts only drive-qualified local paths such as C:\...
// and rejects relative paths, drive-relative paths (C:foo), and UNC paths.
func isLocalWindowsAbsPath(path string) bool {
	if len(path) < 3 {
		return false
	}
	drive := path[0]
	isDrive := (drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')
	if !isDrive || path[1] != ':' || (path[2] != '\\' && path[2] != '/') {
		return false
	}
	// Reject unexpanded environment placeholders like %ProgramFiles% that failed expansion.
	return !strings.Contains(path, "%")
}
