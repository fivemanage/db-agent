package restorejob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/restore"
	"github.com/fivemanage/db-agent/internal/restoreplan"
	"github.com/fivemanage/db-agent/internal/securefile"
)

var ErrExecution = errors.New("restore job failed; inspect its classified local result before requesting a new isolated destination; no production cutover was performed")

type Options struct {
	Timeout          time.Duration
	StatementTimeout time.Duration
	MaxSQLBytes      int64
	Download         restore.Options
	// Observe receives only saved, secret-free state. It must not block on
	// networking; delivery/acknowledgement remains independent of execution.
	Observe func(State)
}

type Runner struct {
	path       string
	journal    *Journal
	lookup     restore.Lookup
	options    Options
	importPlan func(context.Context, config.DatabaseConfig, config.RestoreConfig, *restoreplan.Plan, time.Duration, func(context.Context) error, func(context.Context) error) error
}

func NewRunner(path string, binding Binding, lookup restore.Lookup, options Options) (*Runner, error) {
	if lookup == nil || options.Timeout < 0 || options.StatementTimeout < 0 || options.MaxSQLBytes < 0 || options.MaxSQLBytes > 1<<30 {
		return nil, ErrInvalid
	}
	if options.Timeout == 0 {
		options.Timeout = restore.DefaultTimeout
	}
	if options.MaxSQLBytes == 0 {
		options.MaxSQLBytes = restoreplan.DefaultMaxInputBytes
	}
	if options.Download.MaxOutputBytes == 0 || options.Download.MaxOutputBytes > options.MaxSQLBytes {
		options.Download.MaxOutputBytes = options.MaxSQLBytes
	}
	if _, err := restore.New(lookup, options.Download); err != nil {
		return nil, ErrInvalid
	}
	path, err := filepath.Abs(config.ResolvePath(path))
	if err != nil {
		return nil, ErrState
	}
	root := filepath.Dir(path)
	journal, err := New(filepath.Join(root, "restore-jobs"), filepath.Join(root, "backup-transfer"), binding)
	if err != nil {
		return nil, err
	}
	return &Runner{path: path, journal: journal, lookup: lookup, options: options, importPlan: database.ImportRestore}, nil
}

func (r *Runner) Snapshot(job Job) (State, error) { return r.journal.Read(job) }

// Run returns the saved terminal state without local prerequisites or SQL on
// redelivery. Callers must inspect Phase, not interpret a replay's nil error as
// success. Fresh failures are saved before returning a classified safe error.
func (r *Runner) Run(parent context.Context, job Job) (state State, err error) {
	ctx, cancel := context.WithTimeout(parent, r.options.Timeout)
	defer cancel()
	session, err := r.journal.Open(ctx, job)
	if err != nil {
		return State{}, err
	}
	defer session.Close()
	r.observe(session)
	if state := session.Snapshot(); state.Terminal() {
		return state, nil
	}
	err = r.execute(ctx, session, job)
	if err == nil {
		err = session.Succeed(ctx)
	}
	if err != nil {
		problem := executionProblem(err, session.Snapshot())
		var saved error
		if errors.Is(err, context.Canceled) && !errors.Is(err, database.ErrRestoreUncertain) && !errors.Is(err, errStorage) && !errors.Is(err, restore.ErrCleanup) {
			saved = session.Cancel()
		} else {
			saved = session.Fail(problem)
		}
		if saved != nil {
			return session.Snapshot(), saved
		}
		r.observe(session)
		return session.Snapshot(), &executionError{err}
	}
	r.observe(session)
	return session.Snapshot(), nil
}

func (r *Runner) observe(session *Session) {
	if r.options.Observe != nil {
		r.options.Observe(session.Snapshot())
	}
}

func (r *Runner) execute(ctx context.Context, session *Session, job Job) (err error) {
	settings, err := config.LoadDiagnostic(r.path)
	if err != nil {
		return ErrState
	}
	k, err := restore.ConfiguredKey(r.path, settings.Encryption)
	if err != nil {
		return errOriginalKey
	}
	if err := settings.Restore.ValidateFor(settings.Database); err != nil {
		return err
	}
	if len(settings.Database.Databases) != 1 {
		return restoreplan.ErrUnsupported
	}
	dir, err := securefile.MkdirTemp(r.journal.dir, ".restore-work-")
	if err != nil {
		return errStorage
	}
	output := filepath.Join(dir, "verified.sql")
	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		removeErr, dirErr := os.Remove(output), os.Remove(dir)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) || dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
			err = errStorage
		}
	}()
	if err := session.StartDownload(ctx); err != nil {
		return err
	}
	r.observe(session)
	restorer, err := restore.New(jobLookup{source: r.lookup, job: job}, r.options.Download)
	if err != nil {
		return errStorage
	}
	result, err := restorer.Restore(ctx, restore.Request{BackupID: job.BackupID, Output: output, Key: k})
	if err != nil {
		return err
	}
	if err := session.MarkVerified(ctx, result.Artifact); err != nil {
		return err
	}
	r.observe(session)
	file, err := securefile.Open(output, r.options.MaxSQLBytes)
	if err != nil {
		return errStorage
	}
	plan, planErr := restoreplan.Prepare(ctx, file, restoreplan.Options{Driver: settings.Database.Driver, Source: settings.Database.Databases[0], Destination: job.Destination(), MaxInputBytes: r.options.MaxSQLBytes})
	closeErr := file.Close()
	removeErr := os.Remove(output)
	dirErr := os.Remove(dir)
	if closeErr != nil || removeErr != nil || dirErr != nil {
		return errStorage
	}
	cleaned = true
	if planErr != nil {
		return planErr
	}
	return r.importPlan(ctx, settings.Database, settings.Restore, plan, r.options.StatementTimeout,
		func(ctx context.Context) error {
			// Honor local revocation/reconfiguration before the first account or
			// destination write, not just the config loaded before downloading.
			current, err := config.LoadDiagnostic(r.path)
			if err != nil {
				return ErrState
			}
			a, b := current.Database, settings.Database
			if current.Restore != settings.Restore || a.Driver != b.Driver || a.Host != b.Host || a.Port != b.Port || a.User != b.User || !slices.Equal(a.Databases, b.Databases) {
				return config.ErrRestoreAuthorization
			}
			if current.Encryption != settings.Encryption {
				return errOriginalKey
			}
			if err := session.BeginImport(ctx); err != nil {
				return err
			}
			r.observe(session)
			return nil
		}, func(ctx context.Context) error {
			if err := session.BeginValidation(ctx); err != nil {
				return err
			}
			r.observe(session)
			return nil
		})
}

type jobLookup struct {
	source restore.Lookup
	job    Job
}

func (l jobLookup) DownloadBackup(ctx context.Context, id string) (controlplane.DownloadBackupResponse, error) {
	result, err := l.source.DownloadBackup(ctx, id)
	if err != nil {
		return controlplane.DownloadBackupResponse{}, err
	}
	if id != l.job.BackupID || result.SizeBytes != l.job.SizeBytes || result.Checksum != l.job.Checksum {
		return controlplane.DownloadBackupResponse{}, restore.ErrIntegrity
	}
	return result, nil
}

var errOriginalKey = errors.New("original protected recovery key is unavailable")
var errStorage = errors.New("private local restore workspace is unavailable")

func executionProblem(err error, state State) Problem {
	switch {
	case errors.Is(err, database.ErrRestoreUncertain):
		return ImportInterrupted
	case errors.Is(err, errOriginalKey):
		return OriginalKeyUnavailable
	case errors.Is(err, config.ErrRestoreAuthorization), errors.Is(err, database.ErrRestorePrivileges), errors.Is(err, database.ErrRestoreUnavailable):
		return WriteAuthorizationUnavailable
	case errors.Is(err, restoreplan.ErrUnsupported):
		return UnsafeImport
	case errors.Is(err, database.ErrRestorePolicy):
		return UnsafeImport
	case errors.Is(err, restore.ErrIntegrity), errors.Is(err, restore.ErrFormat), errors.Is(err, ErrTransition):
		return ArtifactInvalid
	case errors.Is(err, errStorage), errors.Is(err, ErrState), errors.Is(err, restore.ErrCleanup):
		return LocalUnavailable
	case errors.Is(err, restore.ErrLimit), errors.Is(err, restoreplan.ErrLimit):
		return ResourceLimit
	case errors.Is(err, database.ErrRestoreValidation):
		return ValidationFailed
	case errors.Is(err, database.ErrRestoreExists):
		return UnsafeImport
	case state.WritesStarted:
		return ImportFailed
	default:
		return DownloadFailed
	}
}

type executionError struct{ cause error }

func (e *executionError) Error() string        { return ErrExecution.Error() }
func (e *executionError) Unwrap() error        { return e.cause }
func (e *executionError) Is(target error) bool { return target == ErrExecution }
