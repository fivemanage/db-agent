package update

import (
	"errors"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/operation"
)

func TestUpdateHandoffWaitsForBackupRestoreOperationLock(t *testing.T) {
	dir := t.TempDir()
	unlock, err := operation.TryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if acquired, err := waitForOperations(dir, time.Millisecond); !errors.Is(err, operation.ErrBusy) {
		if acquired != nil {
			acquired()
		}
		unlock()
		t.Fatal("update helper overlapped an active backup/restore")
	}
	done := make(chan struct{})
	go func() { time.Sleep(20 * time.Millisecond); unlock(); close(done) }()
	release, err := waitForOperations(dir, time.Second)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if next, err := operation.TryLock(dir); !errors.Is(err, operation.ErrBusy) {
		if next != nil {
			next()
		}
		t.Fatal("helper did not hold exclusion during replacement/restart")
	}
}
