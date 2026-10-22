// Package operation serializes local backup and restore work across processes.
package operation

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/fivemanage/db-agent/internal/securefile"
)

var (
	ErrBusy  = errors.New("another agent operation is running; wait for it to finish before retrying")
	ErrState = errors.New("cannot access the private agent operation lock; check local permissions")
)

// TryLock uses the existing backup whole-run lock. Every caller must supply the
// same backup-transfer directory, not its own domain's state directory.
func TryLock(dir string) (func(), error) {
	if dir == "" {
		return nil, ErrState
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, &lockError{err}
	}
	unlock, err := securefile.TryLock(filepath.Join(dir, ".run.lock"))
	if errors.Is(err, securefile.ErrLocked) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, &lockError{err}
	}
	return unlock, nil
}

type lockError struct{ cause error }

func (e *lockError) Error() string        { return ErrState.Error() }
func (e *lockError) Unwrap() error        { return e.cause }
func (e *lockError) Is(target error) bool { return target == ErrState }
