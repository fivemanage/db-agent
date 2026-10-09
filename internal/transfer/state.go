package transfer

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/securefile"
)

var identityPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var backupPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var checksumPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type entry struct {
	Version    int                                `json:"version"`
	Initiation controlplane.InitiateBackupRequest `json:"initiation"`
	RequestID  string                             `json:"request_id"`
	BackupID   string                             `json:"backup_id,omitempty"`
	Phase      string                             `json:"phase"`
	Sessions   int                                `json:"sessions"`
	NotBefore  time.Time                          `json:"not_before,omitempty"`
	Completion controlplane.CompleteBackupRequest `json:"completion"`
	Problem    controlplane.Problem               `json:"problem,omitempty"`
}

func (u *Uploader) statePath() string {
	return filepath.Join(u.dir, "pending.json")
}

func (u *Uploader) artifactPath() string {
	return filepath.Join(u.dir, "ciphertext.bak")
}

func (u *Uploader) load() (*entry, error) {
	data, err := securefile.Read(u.statePath(), 4096)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &safeError{ErrLocal, err}
	}

	var e entry
	if json.Unmarshal(data, &e) != nil || !e.valid() {
		return nil, ErrLocal
	}

	e.Initiation.RequestID = e.RequestID
	e.Completion.Problem = e.Problem

	return &e, nil
}

func (e *entry) valid() bool {
	if e.Version != 1 || !identityPattern.MatchString(e.RequestID) {
		return false
	}
	if e.BackupID != "" && !backupPattern.MatchString(e.BackupID) {
		return false
	}

	initiation := e.Initiation
	if initiation.StartedAt.IsZero() || initiation.StartedAt.After(time.Now().Add(time.Minute)) {
		return false
	}
	if initiation.EstimatedSizeBytes != nil && *initiation.EstimatedSizeBytes < 0 {
		return false
	}

	if e.Phase != "building" && e.Phase != "ready" && e.Phase != "report" {
		return false
	}
	if e.Sessions < 0 || e.Sessions > 3 {
		return false
	}

	completion := e.Completion
	if completion.SizeBytes < 0 || completion.DurationSeconds < 0 ||
		math.IsNaN(completion.DurationSeconds) || math.IsInf(completion.DurationSeconds, 0) {
		return false
	}

	switch completion.Status {
	case "success":
		if !checksumPattern.MatchString(completion.Checksum) || e.Problem != controlplane.NoProblem {
			return false
		}
	case "failed":
		if completion.Checksum != "" ||
			(e.Problem != controlplane.BackupFailed && e.Problem != controlplane.UploadFailed) {
			return false
		}
	default:
		return false
	}

	return e.Phase != "ready" || completion.Status == "success"
}

func (u *Uploader) save(e *entry) error {
	e.Problem = e.Completion.Problem
	if !e.valid() {
		return ErrLocal
	}

	data, err := json.Marshal(e)
	if err != nil || len(data) > 4096 {
		return ErrLocal
	}

	// Only this reserved staging file may be removed after an interrupted save.
	staged := u.statePath() + ".next"
	if err := remove(staged); err != nil {
		return err
	}
	if err := securefile.Create(staged, data); err != nil {
		return &safeError{ErrLocal, err}
	}
	if err := securefile.ReplaceDurable(staged, u.statePath()); err != nil {
		return &safeError{ErrLocal, err}
	}

	return nil
}

func remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &safeError{ErrLocal, err}
	}

	return nil
}
