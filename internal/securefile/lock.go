package securefile

import (
	"errors"
	"fmt"
	"os"
)

var ErrLocked = errors.New("private file is already locked")

// TryLock holds a nonblocking OS lock until release or process exit. The file
// must remain in place: unlinking it would let another process lock a new inode.
func TryLock(path string) (func(), error) {
	file, err := OpenNew(path)
	if err == nil {
		err = file.Close()
	}
	if err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create private lock: %w", err)
	}

	file, err = Open(path, 0)
	if err != nil {
		return nil, fmt.Errorf("open private lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		file.Close()
		return nil, err
	}

	return func() {
		unlockFile(file)
		file.Close()
	}, nil
}
