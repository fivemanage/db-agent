package restorejob

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/operation"
)

type workerPlane struct {
	*runnerLookup
	mu         sync.Mutex
	poll       controlplane.RestorePollResponse
	reports    []controlplane.RestoreReport
	onTerminal context.CancelFunc
}

func TestWorkerDoesNotRecreateMissingOrRolledBackAcknowledgedState(t *testing.T) {
	for _, existing := range []bool{false, true} {
		f := setupRunner(t, runnerSQL)
		if existing {
			session, err := f.runner.journal.Open(context.Background(), f.job)
			if err != nil {
				t.Fatal(err)
			}
			session.Close()
		}
		p := &workerPlane{runnerLookup: f.lookup}
		w, err := NewWorker(f.runner.path, p, Options{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.accept(context.Background(), f.runner, f.job, 6, false)
		if !errors.Is(err, ErrState) {
			t.Fatalf("acknowledged job was treated as fresh: %v", err)
		}
		if len(p.reports) != 0 || f.lookup.calls.Load() != 0 {
			t.Fatal("unreconciled local state reached report or artifact execution")
		}
	}
}

func TestWorkerCancellationBeforeExecutionAndSharedLockExclusion(t *testing.T) {
	f := setupRunner(t, runnerSQL)
	p := &workerPlane{runnerLookup: f.lookup}
	w, err := NewWorker(f.runner.path, p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := operation.TryLock(f.runner.journal.operationDir)
	if err != nil {
		t.Fatal(err)
	}
	_, busy := w.accept(context.Background(), f.runner, f.job, 0, true)
	unlock()
	if !errors.Is(busy, operation.ErrBusy) {
		t.Fatal("restore acceptance overlapped backup/update lock")
	}
	state, err := w.accept(context.Background(), f.runner, f.job, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != Canceled || state.WritesStarted || state.Pending() {
		t.Fatalf("unclean cancellation: %+v", state)
	}
	if f.lookup.calls.Load() != 0 {
		t.Fatal("canceled delivery downloaded an artifact")
	}
}

func (p *workerPlane) PollRestores(ctx context.Context, in controlplane.RestorePollRequest) (controlplane.RestorePollResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.poll, nil
}
func (p *workerPlane) ReportRestore(ctx context.Context, id string, in controlplane.RestoreReport) (controlplane.RestoreReportResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports = append(p.reports, in)
	if in.Phase == "failed" || in.Phase == "canceled" || in.Phase == "succeeded" {
		p.poll.Job = nil
		if p.onTerminal != nil {
			p.onTerminal()
		}
	}
	return controlplane.RestoreReportResponse{Revision: in.Revision, CancelRequested: p.poll.CancelRequested}, nil
}
func TestWorkerAcknowledgesDurableAcceptanceBeforeExecution(t *testing.T) {
	f := setupRunner(t, runnerSQL)
	f.settings.Restore.Authorized = false
	f.save(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := &workerPlane{runnerLookup: f.lookup, poll: controlplane.RestorePollResponse{Protocol: 1, DatabaseID: f.job.DatabaseID, AgentID: f.job.AgentID, Job: &controlplane.RestoreJob{ID: f.job.ID, DatabaseID: f.job.DatabaseID, AgentID: f.job.AgentID, BackupID: f.job.BackupID, SizeBytes: f.job.SizeBytes, Checksum: f.job.Checksum}}, onTerminal: cancel}
	w, err := NewWorker(f.runner.path, p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	w.interval = time.Millisecond
	w.readiness = func(context.Context, string) string { return "ready" }
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reports) < 2 || p.reports[0].Phase != "accepted" || p.reports[len(p.reports)-1].Phase != "failed" || p.reports[len(p.reports)-1].Problem != string(WriteAuthorizationUnavailable) {
		t.Fatalf("wrong acceptance/execution/report order: %+v", p.reports)
	}
	if f.gets.Load() != 0 {
		t.Fatal("unready local authorization allowed artifact execution")
	}
}
func TestWorkerTerminalOutboxRecoversLostAcknowledgement(t *testing.T) {
	f := setupRunner(t, runnerSQL)
	s, err := f.runner.journal.Open(context.Background(), f.job)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(OriginalKeyUnavailable); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := &workerPlane{runnerLookup: f.lookup, poll: controlplane.RestorePollResponse{Protocol: 1, DatabaseID: f.job.DatabaseID, AgentID: f.job.AgentID}, onTerminal: cancel}
	w, _ := NewWorker(f.runner.path, p, Options{})
	w.readiness = func(context.Context, string) string { return "ready" }
	_ = w.Run(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reports) != 1 || !reflect.DeepEqual(p.reports[0], controlplane.RestoreReport{Revision: before.Revision, Phase: string(before.Phase), Problem: string(before.Problem)}) {
		t.Fatal("pending immutable terminal state was not replayed after restart")
	}
	if f.lookup.calls.Load() != 0 {
		t.Fatal("outbox recovery redownloaded/reimported")
	}
}
