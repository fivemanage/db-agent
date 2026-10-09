package transfer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/securefile"
)

type ControlPlane interface {
	InitiateBackup(context.Context, controlplane.InitiateBackupRequest) (controlplane.InitiateBackupResponse, error)
	CompleteBackup(context.Context, string, controlplane.CompleteBackupRequest) error
}

type Options struct {
	MaxBytes         int64
	BytesPerSecond   int64
	MaxAge           time.Duration
	SessionTimeout   time.Duration
	FreeSpaceReserve int64
}

type Uploader struct {
	dir     string
	plane   ControlPlane
	options Options
	http    *http.Client
	mu      sync.Mutex
}

func New(dir string, plane ControlPlane, options Options) (*Uploader, error) {
	if dir == "" || plane == nil ||
		options.MaxBytes < 0 || options.BytesPerSecond < 0 ||
		options.MaxAge < 0 || options.SessionTimeout < 0 || options.FreeSpaceReserve < 0 {
		return nil, errors.New("invalid backup transfer settings")
	}

	if options.MaxBytes == 0 {
		options.MaxBytes = 1 << 30
	}
	if options.MaxAge == 0 {
		options.MaxAge = 24 * time.Hour
	}
	if options.SessionTimeout == 0 {
		options.SessionTimeout = 30 * time.Minute
	}
	if options.FreeSpaceReserve == 0 {
		options.FreeSpaceReserve = 256 << 20
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second

	return &Uploader{
		dir:     dir,
		plane:   plane,
		options: options,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Run owns and closes source, including on cancellation. Closing source must
// unblock Read; the orchestrator still owns joining its dump process workers.
// No raw dump bytes can reach disk or the control plane through this API.
func (u *Uploader) Run(ctx context.Context, source io.ReadCloser, k key.Key, compression artifact.Compression, estimate *int64) error {
	if source == nil {
		return errors.New("backup source is required")
	}
	defer source.Close()

	if !u.mu.TryLock() {
		return ErrBusy
	}
	defer u.mu.Unlock()

	unlock, err := u.lock()
	if err != nil {
		return err
	}
	defer unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if estimate != nil && *estimate < 0 {
		return errors.New("backup estimate must be nonnegative")
	}

	previous, err := u.load()
	if err != nil {
		return err
	}
	if previous != nil {
		return ErrBusy
	}

	id, err := controlplane.NewRequestID()
	if err != nil {
		return err
	}

	e := &entry{
		Version:   1,
		RequestID: id,
		Phase:     "building",
		Initiation: controlplane.InitiateBackupRequest{
			RequestID:          id,
			StartedAt:          time.Now().UTC(),
			EstimatedSizeBytes: estimate,
		},
		Completion: controlplane.CompleteBackupRequest{
			Status:  "failed",
			Problem: controlplane.BackupFailed,
		},
	}

	// The failed intent is durable before writing any ciphertext. A crash while
	// building can only become a failed report, never a successful partial upload.
	if err := u.save(e); err != nil {
		return err
	}

	metadata, err := u.stage(ctx, source, k, compression)
	if err != nil {
		e.Completion.DurationSeconds = time.Since(e.Initiation.StartedAt).Seconds()
		e.Phase = "report"

		if saveErr := u.save(e); saveErr != nil {
			return saveErr
		}
		if cleanupErr := remove(u.artifactPath()); cleanupErr != nil {
			return cleanupErr
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		return errors.Join(&safeError{ErrBackup, err}, u.resume(ctx, e))
	}

	e.Phase = "ready"
	e.Completion = controlplane.CompleteBackupRequest{
		Status:    "success",
		SizeBytes: metadata.SizeBytes,
		Checksum:  metadata.Checksum,
	}
	if err := u.save(e); err != nil {
		return err
	}

	return u.resume(ctx, e)
}

func (u *Uploader) stage(ctx context.Context, source io.ReadCloser, k key.Key, compression artifact.Compression) (artifact.Metadata, error) {
	if err := os.MkdirAll(u.dir, 0700); err != nil {
		return artifact.Metadata{}, err
	}
	if err := remove(u.artifactPath()); err != nil {
		return artifact.Metadata{}, err
	}

	file, err := securefile.OpenNew(u.artifactPath())
	if err != nil {
		return artifact.Metadata{}, err
	}
	defer file.Close()

	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		source.Close()
		close(closed)
	})
	defer func() {
		if !stop() {
			<-closed
		}
	}()

	destination := &limitedWriter{
		dst:       file,
		remaining: u.options.MaxBytes,
		reserve:   u.options.FreeSpaceReserve,
		free: func() (uint64, error) {
			return freeSpace(u.dir)
		},
	}
	metadata, err := artifact.Encrypt(ctx, destination, source, k, compression)
	if err != nil {
		return artifact.Metadata{}, err
	}

	if err := file.Sync(); err != nil {
		return artifact.Metadata{}, err
	}
	if err := file.Close(); err != nil {
		return artifact.Metadata{}, err
	}

	return metadata, nil
}

type limitedWriter struct {
	dst       io.Writer
	remaining int64
	reserve   int64
	free      func() (uint64, error)
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrLimit
	}

	available, err := w.free()
	if err != nil {
		return 0, &safeError{ErrLocal, err}
	}
	if available < uint64(w.reserve) || uint64(len(p)) > available-uint64(w.reserve) {
		return 0, ErrLimit
	}

	n, err := w.dst.Write(p)
	w.remaining -= int64(n)

	return n, err
}

// Resume is called after a successful heartbeat/session. It does at most three
// upload attempts, or replays the exact durable terminal report once (the client
// itself retries transient API failures). A busy active run isn't interrupted.
func (u *Uploader) Resume(ctx context.Context) error {
	if !u.mu.TryLock() {
		return ErrBusy
	}
	defer u.mu.Unlock()

	unlock, err := u.lock()
	if err != nil {
		return err
	}
	defer unlock()

	e, err := u.load()
	if err != nil || e == nil {
		return err
	}

	return u.resume(ctx, e)
}

func (u *Uploader) lock() (func(), error) {
	if err := os.MkdirAll(u.dir, 0700); err != nil {
		return nil, &safeError{ErrLocal, err}
	}

	path := filepath.Join(u.dir, ".lock")
	file, err := securefile.OpenNew(path)
	if err == nil {
		err = file.Close()
	}
	if err != nil && !errors.Is(err, os.ErrExist) {
		return nil, &safeError{ErrLocal, err}
	}

	file, err = securefile.Open(path, 0)
	if err != nil {
		return nil, &safeError{ErrLocal, err}
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

func (u *Uploader) resume(parent context.Context, e *entry) error {
	ctx, cancel := context.WithTimeout(parent, u.options.SessionTimeout)
	defer cancel()

	if e.Phase == "report" {
		return u.deliver(ctx, e)
	}
	if e.Phase == "building" {
		return u.fail(ctx, e, controlplane.BackupFailed)
	}

	if time.Since(e.Initiation.StartedAt) >= u.options.MaxAge || e.Sessions >= 3 {
		return u.fail(ctx, e, controlplane.UploadFailed)
	}
	if parent.Err() != nil {
		return errors.Join(parent.Err(), u.fail(parent, e, controlplane.UploadFailed))
	}
	if time.Now().Before(e.NotBefore) {
		return ErrPending
	}

	e.Sessions++
	if err := u.save(e); err != nil {
		return err
	}

	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			break
		}

		// Replaying initiation also renews an expired URL, without creating a new
		// backup. The immutable request metadata and identity are never changed.
		target, err := u.plane.InitiateBackup(ctx, e.Initiation)
		if err != nil {
			if errors.Is(err, controlplane.ErrCredentialRejected) {
				return err
			}

			break
		}
		if !backupPattern.MatchString(target.BackupID) || (e.BackupID != "" && target.BackupID != e.BackupID) {
			return ErrLocal
		}

		e.BackupID = target.BackupID
		e.NotBefore = time.Time{}
		if err := u.save(e); err != nil {
			return err
		}

		retry, retryAfter, err := u.put(ctx, e, target)
		if err == nil {
			e.Phase = "report"
			e.Completion.DurationSeconds = time.Since(e.Initiation.StartedAt).Seconds()
			if err := u.save(e); err != nil {
				return err
			}

			return u.deliver(ctx, e)
		}
		if !retry {
			return errors.Join(err, u.fail(ctx, e, controlplane.UploadFailed))
		}

		delay := retryDelay(attempt, retryAfter)
		if retryAfter != "" {
			e.NotBefore = time.Now().Add(delay)
			if err := u.save(e); err != nil {
				return err
			}
		}
		if attempt < 2 {
			if err := wait(ctx, delay); err != nil {
				break
			}
		}
	}

	if parent.Err() != nil {
		return errors.Join(parent.Err(), u.fail(parent, e, controlplane.UploadFailed))
	}
	if e.Sessions >= 3 {
		return errors.Join(ErrUpload, u.fail(ctx, e, controlplane.UploadFailed))
	}

	return ErrPending
}

func (u *Uploader) fail(ctx context.Context, e *entry, problem controlplane.Problem) error {
	e.Phase = "report"
	e.Completion = controlplane.CompleteBackupRequest{
		Status:          "failed",
		DurationSeconds: time.Since(e.Initiation.StartedAt).Seconds(),
		Problem:         problem,
	}
	if err := u.save(e); err != nil {
		return err
	}

	return u.deliver(ctx, e)
}

func (u *Uploader) deliver(ctx context.Context, e *entry) error {
	// The report is committed before freeing the artifact or contacting the API.
	if err := remove(u.artifactPath()); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if e.BackupID == "" {
		target, err := u.plane.InitiateBackup(ctx, e.Initiation)
		if err != nil {
			return &safeError{ErrPending, err}
		}

		e.BackupID = target.BackupID
		if err := u.save(e); err != nil {
			return err
		}
	}

	if err := u.plane.CompleteBackup(ctx, e.BackupID, e.Completion); err != nil {
		return &safeError{ErrPending, err}
	}

	return remove(u.statePath())
}
