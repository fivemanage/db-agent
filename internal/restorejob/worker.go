package restorejob

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/operation"
	"github.com/fivemanage/db-agent/internal/restore"
)

const PollInterval = 15 * time.Second

type ControlPlane interface {
	restore.Lookup
	PollRestores(context.Context, controlplane.RestorePollRequest) (controlplane.RestorePollResponse, error)
	ReportRestore(context.Context, string, controlplane.RestoreReport) (controlplane.RestoreReportResponse, error)
}

type Worker struct {
	path      string
	plane     ControlPlane
	options   Options
	interval  time.Duration
	readiness func(context.Context, string) string
}

func NewWorker(path string, plane ControlPlane, options Options) (*Worker, error) {
	if path == "" || plane == nil {
		return nil, ErrInvalid
	}
	return &Worker{path: path, plane: plane, options: options, interval: PollInterval, readiness: localReadiness}, nil
}

func localReadiness(ctx context.Context, path string) string {
	settings, err := config.LoadDiagnostic(path)
	if err != nil {
		return string(LocalUnavailable)
	}
	if _, err := restore.ConfiguredKey(path, settings.Encryption); err != nil {
		return string(OriginalKeyUnavailable)
	}
	if len(settings.Database.Databases) != 1 {
		return string(UnsafeImport)
	}
	if err := database.CheckRestoreAccess(ctx, settings.Database, settings.Restore); err != nil {
		return string(WriteAuthorizationUnavailable)
	}
	return "ready"
}

type runResult struct {
	state State
	err   error
}

// Run owns and joins at most one local executor. Reports/retries are outbound,
// independent of the executor and heartbeat; no network call holds the SQL lock.
func (w *Worker) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	updates := make(chan State, 1)
	done := make(chan runResult, 1)
	var runner *Runner
	var job *Job
	var stopJob context.CancelFunc
	running := false
	defer func() {
		if running {
			stopJob()
			<-done
		}
	}()
	poll := true
	for {
		if ctx.Err() != nil {
			return nil
		}
		if poll {
			input := controlplane.RestorePollRequest{Protocol: controlplane.RestoreProtocol, Readiness: w.readiness(ctx, w.path)}
			if job != nil {
				input.ActiveJobID = job.ID
			}
			response, err := w.plane.PollRestores(ctx, input)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				var httpErr *controlplane.ResponseError
				if runner == nil && errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode >= 300 && httpErr.StatusCode < 400) {
					return nil
				}
				if !transient(err) {
					return err
				}
			} else {
				binding := Binding{DatabaseID: response.DatabaseID, AgentID: response.AgentID}
				if runner == nil || runner.journal.binding != binding {
					if running {
						return ErrBinding
					}
					options := w.options
					previous := options.Observe
					options.Observe = func(state State) {
						if previous != nil {
							previous(state)
						}
						select {
						case updates <- state:
						default:
							select {
							case <-updates:
							default:
							}
							select {
							case updates <- state:
							default:
							}
						}
					}
					runner, err = NewRunner(w.path, binding, w.plane, options)
					if err != nil {
						return err
					}
				}
				if !running {
					pending, err := runner.journal.pendingTerminal()
					if err != nil {
						return err
					}
					for _, state := range pending {
						_, err := w.deliver(ctx, runner, state)
						if err != nil && !transient(err) && !errors.Is(err, operation.ErrBusy) {
							return err
						}
					}
				}
				if response.Job != nil {
					value := Job{ID: response.Job.ID, Binding: binding, BackupID: response.Job.BackupID, SizeBytes: response.Job.SizeBytes, Checksum: response.Job.Checksum}
					if job != nil && *job != value {
						return ErrConflict
					}
					job = &value
					if running {
						if response.CancelRequested {
							stopJob()
						}
					} else {
						state, err := w.accept(ctx, runner, value, response.Job.Revision, response.CancelRequested)
						if err != nil {
							if !transient(err) && !errors.Is(err, operation.ErrBusy) {
								return err
							}
						} else if state.Terminal() {
							job = nil
						} else {
							running = true
							stopJob = startRun(ctx, runner, value, done)
						}
					}
				} else if !running {
					job = nil
				}
			}
			poll = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			poll = true
		case state := <-updates:
			if running && job != nil && state.Job == *job && !state.Terminal() {
				response, err := w.report(ctx, state)
				if err == nil && response.CancelRequested {
					stopJob()
				}
				if err != nil && !transient(err) {
					return err
				}
			}
		case result := <-done:
			running = false
			stopJob()
			if errors.Is(result.err, operation.ErrBusy) {
				poll = false
				continue
			}
			if !result.state.Terminal() {
				if ctx.Err() != nil {
					return nil
				}
				return ErrState
			}
			_, err := w.deliver(ctx, runner, result.state)
			if err == nil {
				job = nil
			} else if !transient(err) && !errors.Is(err, operation.ErrBusy) {
				return err
			}
			poll = true
		}
	}
}

func startRun(ctx context.Context, runner *Runner, job Job, done chan<- runResult) context.CancelFunc {
	work, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		state, err := runner.Run(work, job)
		done <- runResult{state, err}
	}()
	return cancel
}

func (w *Worker) report(ctx context.Context, state State) (controlplane.RestoreReportResponse, error) {
	return w.plane.ReportRestore(ctx, state.Job.ID, controlplane.RestoreReport{Revision: state.Revision, Phase: string(state.Phase), WritesStarted: state.WritesStarted, Problem: string(state.Problem)})
}

func (w *Worker) deliver(ctx context.Context, runner *Runner, state State) (bool, error) {
	result, err := w.report(ctx, state)
	if err != nil {
		return false, err
	}
	if state.Terminal() {
		session, err := runner.journal.Open(ctx, state.Job)
		if err != nil {
			return result.CancelRequested, err
		}
		defer session.Close()
		if err := session.Acknowledge(result.Revision); err != nil {
			return result.CancelRequested, err
		}
	}
	return result.CancelRequested, nil
}

func (w *Worker) accept(ctx context.Context, runner *Runner, job Job, remoteRevision uint64, canceled bool) (State, error) {
	// A server acknowledgement proves private acceptance existed. Missing or
	// rolled-back local state is not a fresh job, even if the destination is gone.
	if remoteRevision > 0 {
		state, err := runner.Snapshot(job)
		if err != nil || state.Revision < remoteRevision {
			return State{}, ErrState
		}
	}
	session, err := runner.journal.Open(ctx, job)
	if err != nil {
		return State{}, err
	}
	if canceled && !session.Snapshot().Terminal() {
		err = session.Cancel()
	}
	state := session.Snapshot()
	session.Close()
	if err != nil {
		return state, err
	}
	response, err := w.report(ctx, state)
	if err != nil {
		return state, err
	}
	session, err = runner.journal.Open(ctx, job)
	if err != nil {
		return state, err
	}
	defer session.Close()
	if response.CancelRequested && !session.Snapshot().Terminal() {
		if err := session.Cancel(); err != nil {
			return session.Snapshot(), err
		}
		state = session.Snapshot()
		// Terminal report is sent without the operation lock by the pending
		// outbox on the next poll; no import starts after this cancellation.
		return state, nil
	}
	err = session.Acknowledge(response.Revision)
	return session.Snapshot(), err
}

func transient(err error) bool {
	var response *controlplane.ResponseError
	var transport *controlplane.TransportError
	return errors.As(err, &transport) || errors.As(err, &response) && (response.StatusCode == 429 || response.StatusCode >= 500)
}
