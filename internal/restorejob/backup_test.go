package restorejob

import (
	"context"
	"errors"
	"testing"

	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/operation"
	"github.com/fivemanage/db-agent/internal/transfer"
)

type unusedPlane struct{ t *testing.T }

func (p unusedPlane) InitiateBackup(context.Context, controlplane.InitiateBackupRequest) (controlplane.InitiateBackupResponse, error) {
	p.t.Error("backup network request escaped the operation lock")
	return controlplane.InitiateBackupResponse{}, errors.New("unexpected request")
}

func (p unusedPlane) CompleteBackup(context.Context, string, controlplane.CompleteBackupRequest) error {
	p.t.Error("backup network request escaped the operation lock")
	return errors.New("unexpected request")
}

func TestRestoreSessionSharesExistingBackupLock(t *testing.T) {
	j := newJournal(t)
	uploader, err := transfer.New(j.operationDir, unusedPlane{t}, transfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := backup.New(j.operationDir, uploader)
	if err != nil {
		t.Fatal(err)
	}
	s := openJob(t, j, testJob())
	prepared := false
	err = runner.Run(context.Background(), func(context.Context) (backup.Plan, error) {
		prepared = true
		return backup.Plan{}, nil
	})
	if !errors.Is(err, backup.ErrRunning) || prepared {
		t.Fatalf("backup preparation overlapped restore: %v", err)
	}
	if err := runner.Resume(context.Background()); !errors.Is(err, backup.ErrRunning) {
		t.Fatalf("backup recovery overlapped restore: %v", err)
	}
	s.Close()

	blocked := false
	err = runner.Run(context.Background(), func(context.Context) (backup.Plan, error) {
		next, err := j.Open(context.Background(), testJob())
		if next != nil {
			next.Close()
		}
		blocked = errors.Is(err, operation.ErrBusy)
		return backup.Plan{}, errors.New("stop before dumping")
	})
	if !errors.Is(err, backup.ErrFailed) || !blocked {
		t.Fatalf("restore overlapped backup preparation: %v", err)
	}
	openJob(t, j, testJob())
}
