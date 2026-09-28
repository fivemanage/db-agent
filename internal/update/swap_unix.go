//go:build !windows

package update

import (
	"io"
	"os"
)

func replaceWithBackup(target, staged, backup string) error {
	old, err := os.Open(target)
	if err != nil {
		return err
	}
	defer old.Close()
	info, err := old.Stat()
	if err != nil {
		return err
	}
	copy, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	valid := false
	defer func() {
		copy.Close()
		if !valid {
			os.Remove(backup)
		}
	}()
	if _, err := io.Copy(copy, old); err != nil {
		return err
	}
	if err := copy.Sync(); err != nil {
		return err
	}
	if err := copy.Close(); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return err
	}
	valid = true
	return nil
}

func restoreBackup(target, backup string) error {
	return os.Rename(backup, target)
}
