package transfer

import (
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
)

// Progress contains only durable, non-secret run metadata. Acknowledged means
// the terminal report was accepted, not merely that the PUT finished.
type Progress struct {
	RequestID    string
	BackupID     string
	StartedAt    time.Time
	Phase        string
	Completion   controlplane.CompleteBackupRequest
	Acknowledged bool
}

type session struct {
	*Uploader
	observe func(Progress) error
}

func (s *session) notify(e *entry, acknowledged bool) error {
	if s.observe == nil {
		return nil
	}
	if err := s.observe(Progress{
		RequestID: e.RequestID, BackupID: e.BackupID, StartedAt: e.Initiation.StartedAt,
		Phase: e.Phase, Completion: e.Completion, Acknowledged: acknowledged,
	}); err != nil {
		return &safeError{ErrLocal, err}
	}
	return nil
}

func (s *session) save(e *entry) error {
	if err := s.Uploader.save(e); err != nil {
		return err
	}
	return s.notify(e, false)
}
