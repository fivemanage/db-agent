//go:build !windows

package securefile

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func OpenNew(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

func openRead(path string) (*os.File, error) {
	// O_NONBLOCK also makes a FIFO swapped in after Lstat fail without hanging.
	return os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}

func checkPermissions(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return ErrUnsafe
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func publish(from, to string) error {
	if err := os.Link(from, to); err != nil {
		return err
	}
	return syncDirectory(to)
}

func Replace(from, to string) error {
	return os.Rename(from, to)
}
