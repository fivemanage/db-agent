//go:build !windows

package securefile

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockFile(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return ErrLocked
		}
		return err
	}
	return nil
}

func unlockFile(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
