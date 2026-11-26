//go:build !windows

package database

import (
	"os"
	"path/filepath"
)

func findOffPathCandidates(names []string) []candidateBinary {
	dirs := []string{
		"/usr/bin",
		"/usr/local/bin",
		"/usr/local/mysql/bin",
		"/opt/mariadb/bin",
		"/opt/homebrew/bin",
	}

	var results []candidateBinary
	for _, dir := range dirs {
		dirTrusted := isTrustedUnixPath(dir)
		for _, name := range names {
			fullPath := filepath.Join(dir, name)
			info, err := os.Lstat(fullPath)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			// Must be executable and not world-writable.
			if info.Mode().Perm()&0o111 == 0 {
				continue
			}
			trusted := dirTrusted && info.Mode().Perm()&0o002 == 0
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

func isTrustedUnixPath(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o002 == 0
}

// AddToSystemPath is only implemented for Windows System PATH registry updates.
func AddToSystemPath(string) error {
	return ErrSystemPathUnsupported
}
