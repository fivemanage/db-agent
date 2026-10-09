//go:build !windows

package transfer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func freeSpace(path string) (uint64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}

	return uint64(stats.Bavail) * uint64(stats.Bsize), nil
}

func lockFile(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return ErrBusy
		}

		return &safeError{ErrLocal, err}
	}

	return nil
}

func unlockFile(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
