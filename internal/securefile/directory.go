package securefile

import "os"

// MkdirTemp protects a new workspace before any sensitive output is written.
func MkdirTemp(parent, pattern string) (string, error) {
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	if err := protectDirectory(dir); err != nil {
		os.Remove(dir)
		return "", err
	}
	return dir, nil
}
