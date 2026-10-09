package backup

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
	"github.com/fivemanage/db-agent/internal/transfer"
)

var requestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var backupPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var checksumPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Snapshot struct {
	Version   int                  `json:"version"`
	Activity  controlplane.Status  `json:"activity"`
	LastError controlplane.Problem `json:"last_error,omitempty"`
	Latest    *Run                 `json:"latest,omitempty"`
}

type Run struct {
	RequestID       string    `json:"request_id"`
	BackupID        string    `json:"backup_id,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	Status          string    `json:"status"`
	SizeBytes       int64     `json:"size_bytes"`
	Checksum        string    `json:"checksum,omitempty"`
	DurationSeconds float64   `json:"duration_seconds"`
	Pending         bool      `json:"pending"`
}

func ReadSnapshot(dir string) (Snapshot, error) {
	if dir == "" {
		return Snapshot{}, ErrState
	}
	return (&Runner{dir: dir}).Snapshot()
}

// Snapshot reads shared atomic state, so a service heartbeat can see manual runs
// in another process. An abandoned active phase isn't reported as still running.
func (r *Runner) Snapshot() (Snapshot, error) {
	snapshot, err := r.load()
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.Activity == controlplane.BackingUp || snapshot.Activity == controlplane.Uploading {
		unlock, err := r.lock()
		if err == nil {
			defer unlock()
			// Re-read after acquiring the lock: the run may just have finished.
			snapshot, err = r.load()
			if err != nil {
				return Snapshot{}, err
			}
			if snapshot.Activity == controlplane.BackingUp || snapshot.Activity == controlplane.Uploading {
				snapshot.LastError = controlplane.BackupFailed
				if snapshot.Activity == controlplane.Uploading {
					snapshot.LastError = controlplane.UploadFailed
				}
				snapshot.Activity = controlplane.Error
			}
		} else if !errors.Is(err, ErrRunning) {
			return Snapshot{}, err
		}
	}
	return snapshot, nil
}

func (r *Runner) HeartbeatState(fallback controlplane.State) controlplane.State {
	snapshot, err := r.Snapshot()
	if err != nil {
		fallback.Status, fallback.Problem = controlplane.Error, controlplane.BackupFailed
		return fallback
	}
	if snapshot.Latest == nil && snapshot.LastError == controlplane.NoProblem {
		return fallback
	}
	// Current work takes precedence over an old startup diagnostic. Between runs,
	// incomplete local setup remains a blocker even after an earlier success.
	if fallback.Status == controlplane.Error && snapshot.Activity == controlplane.Idle {
		return fallback
	}
	fallback.Status, fallback.Problem = snapshot.Activity, controlplane.NoProblem
	if snapshot.Activity == controlplane.Error {
		fallback.Problem = snapshot.LastError
	}
	return fallback
}

func (r *Runner) statePath() string { return filepath.Join(r.dir, "run.json") }

func (r *Runner) load() (Snapshot, error) {
	var data []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		data, err = securefile.Read(r.statePath(), 4096)
		// Atomic replacement can change the inode between securefile's Lstat
		// and Open. Retry that race; an actually unsafe file still fails closed.
		if !errors.Is(err, securefile.ErrUnsafe) {
			break
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{Version: 1, Activity: controlplane.Idle}, nil
	}
	if err != nil {
		return Snapshot{}, &safeError{ErrState, err}
	}
	var snapshot Snapshot
	if json.Unmarshal(data, &snapshot) != nil || !snapshot.valid() {
		return Snapshot{}, ErrState
	}
	return snapshot, nil
}

func (s Snapshot) valid() bool {
	if s.Version != 1 {
		return false
	}
	switch s.Activity {
	case controlplane.Idle, controlplane.BackingUp, controlplane.Uploading, controlplane.Error:
	default:
		return false
	}
	switch s.LastError {
	case controlplane.NoProblem, controlplane.SetupIncomplete, controlplane.DatabaseUnavailable, controlplane.BackupFailed, controlplane.UploadFailed:
	default:
		return false
	}
	if s.Latest == nil {
		return s.Activity == controlplane.Idle || s.Activity == controlplane.Error
	}
	run := s.Latest
	if !requestPattern.MatchString(run.RequestID) || run.StartedAt.IsZero() ||
		(run.BackupID != "" && !backupPattern.MatchString(run.BackupID)) ||
		run.SizeBytes < 0 || run.DurationSeconds < 0 || math.IsNaN(run.DurationSeconds) || math.IsInf(run.DurationSeconds, 0) {
		return false
	}
	switch run.Status {
	case "backing_up", "uploading", "failed":
		return run.Checksum == ""
	case "success":
		return checksumPattern.MatchString(run.Checksum)
	default:
		return false
	}
}

func (r *Runner) save(snapshot Snapshot) error {
	if !snapshot.valid() {
		return ErrState
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > 4096 {
		return ErrState
	}
	staged := r.statePath() + ".next"
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &safeError{ErrState, err}
	}
	if err := securefile.Create(staged, data); err != nil {
		return &safeError{ErrState, err}
	}
	defer os.Remove(staged)
	if err := securefile.ReplaceDurable(staged, r.statePath()); err != nil {
		return &safeError{ErrState, err}
	}
	return nil
}

func (r *Runner) observe(progress transfer.Progress) error {
	snapshot := Snapshot{Version: 1, Latest: &Run{
		RequestID: progress.RequestID, BackupID: progress.BackupID, StartedAt: progress.StartedAt, Pending: !progress.Acknowledged,
	}}
	switch progress.Phase {
	case "building":
		snapshot.Activity, snapshot.Latest.Status = controlplane.BackingUp, "backing_up"
	case "ready":
		snapshot.Activity, snapshot.Latest.Status = controlplane.Uploading, "uploading"
		snapshot.Latest.SizeBytes = progress.Completion.SizeBytes
	case "report":
		completion := progress.Completion
		snapshot.Latest.Status = completion.Status
		snapshot.Latest.SizeBytes, snapshot.Latest.Checksum = completion.SizeBytes, completion.Checksum
		snapshot.Latest.DurationSeconds = completion.DurationSeconds
		snapshot.Activity = controlplane.Uploading
		if progress.Acknowledged {
			snapshot.Activity = controlplane.Idle
		}
		if completion.Status == "failed" {
			snapshot.Activity, snapshot.LastError = controlplane.Error, completion.Problem
		}
	default:
		return ErrState
	}
	return r.save(snapshot)
}

func (r *Runner) markError(problem controlplane.Problem) error {
	snapshot, err := r.load()
	if err != nil {
		return err
	}
	snapshot.Activity, snapshot.LastError = controlplane.Error, problem
	return r.save(snapshot)
}
