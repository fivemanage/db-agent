// Package backup coordinates whole backup runs and their shared local status.
package backup

import (
	"context"
	"errors"
	"io"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/operation"
	"github.com/fivemanage/db-agent/internal/transfer"
)

var (
	ErrRunning = errors.New("a backup or restore is already running; wait for it to finish before retrying db-agent backup --now")
	ErrQueued  = errors.New("a backup upload or completion is still queued; restore control-plane connectivity and let the agent retry before starting another backup")
	ErrState   = errors.New("cannot access private backup run state; check permissions and run db-agent test")
	ErrFailed  = errors.New("backup failed; run db-agent test and check local backup status before retrying")
	ErrSpace   = errors.New("backup stopped at the local spool or free-space limit; free disk space or reduce the backup scope before retrying")
)

type Plan struct {
	Dump        func(context.Context, io.Writer) error
	Key         key.Key
	Compression artifact.Compression
}

type Runner struct {
	dir      string
	uploader *transfer.Uploader
}

func New(dir string, uploader *transfer.Uploader) (*Runner, error) {
	if dir == "" || uploader == nil {
		return nil, errors.New("backup runner requires local state and an uploader")
	}
	return &Runner{dir: dir, uploader: uploader}, nil
}

// Run prepares under the same lock used by replay and automatic triggers.
// Dump must respect cancellation; closing the pipe unblocks downstream writes.
func (r *Runner) Run(ctx context.Context, prepare func(context.Context) (Plan, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := r.load(); err != nil {
		return err
	}
	if prepare == nil {
		return ErrFailed
	}

	plan, err := prepare(ctx)
	if err != nil {
		return errors.Join(&safeError{ErrFailed, err}, r.markError(controlplane.SetupIncomplete))
	}
	if plan.Dump == nil {
		return ErrFailed
	}

	source := newSource(ctx, plan.Dump)
	err = r.uploader.RunWithProgress(ctx, source, plan.Key, plan.Compression, nil, r.observe)
	source.Close()
	if errors.Is(err, transfer.ErrBusy) {
		return &safeError{ErrQueued, err}
	}
	if source.err != nil {
		err = errors.Join(err, source.err)
	}
	if err != nil {
		return r.failure(err)
	}

	snapshot, err := r.load()
	if err != nil {
		return err
	}
	if snapshot.Latest != nil && snapshot.Latest.Status == "failed" {
		return ErrFailed
	}
	return nil
}

// Resume never executes another dump. It retries only the existing ciphertext or
// exact completion metadata, under the whole-run lock rather than just PUT's lock.
func (r *Runner) Resume(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := r.load(); err != nil {
		return err
	}

	if err := r.uploader.ResumeWithProgress(ctx, r.observe); err != nil {
		if errors.Is(err, transfer.ErrBusy) {
			return &safeError{ErrQueued, err}
		}
		return r.failure(err)
	}
	return nil
}

func (r *Runner) lock() (func(), error) {
	unlock, err := operation.TryLock(r.dir)
	if errors.Is(err, operation.ErrBusy) {
		return nil, &safeError{ErrRunning, err}
	}
	if err != nil {
		return nil, &safeError{ErrState, err}
	}
	return unlock, nil
}

func (r *Runner) failure(err error) error {
	message := ErrFailed
	problem := controlplane.BackupFailed
	if errors.Is(err, transfer.ErrPending) || errors.Is(err, transfer.ErrUpload) {
		problem = controlplane.UploadFailed
	}
	if errors.Is(err, transfer.ErrPending) {
		message = ErrQueued
	}
	if errors.Is(err, transfer.ErrLimit) {
		message = ErrSpace
	}
	if errors.Is(err, transfer.ErrBackup) {
		problem = controlplane.BackupFailed
	}
	if snapshot, loadErr := r.load(); loadErr == nil && snapshot.Latest != nil {
		if snapshot.Latest.Status == "failed" && snapshot.LastError != controlplane.NoProblem {
			problem = snapshot.LastError
		} else if snapshot.Latest.Status == "success" && snapshot.Latest.Pending {
			problem = controlplane.UploadFailed
		}
	}
	if errors.Is(err, transfer.ErrLocal) || errors.Is(err, ErrState) {
		return errors.Join(&safeError{ErrState, err}, r.markError(problem))
	}
	return errors.Join(&safeError{message, err}, r.markError(problem))
}

type safeError struct {
	message error
	cause   error
}

func (e *safeError) Error() string        { return e.message.Error() }
func (e *safeError) Unwrap() error        { return e.cause }
func (e *safeError) Is(target error) bool { return target == e.message }
