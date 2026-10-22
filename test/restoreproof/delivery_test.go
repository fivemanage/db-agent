//go:build integration

package restoreproof

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/restore"
	"github.com/fivemanage/db-agent/internal/restorejob"
	"gopkg.in/yaml.v3"
)

func TestOutboundWorkerDeliversAndAcknowledgesARealRestore(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.exec(f.admin, "CREATE VIEW "+qi(f.source)+".delivered_balances AS SELECT id,credits FROM "+qi(f.source)+".players")
	x := newExecutionFixture(f, f.nativeDump())
	registerExecutionCleanup(x)
	originalAPI := x.api.Config.Handler
	var mu sync.Mutex
	var reports []controlplane.RestoreReport
	terminal := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+x.settings.AgentCredential {
			t.Error("delivery protocol omitted agent authentication")
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/agent/restores/poll":
			var input controlplane.RestorePollRequest
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.Protocol != 1 {
				t.Error("invalid poll")
				w.WriteHeader(400)
				return
			}
			var job *controlplane.RestoreJob
			if len(reports) == 0 || reports[len(reports)-1].Phase != "succeeded" || input.ActiveJobID != "" {
				job = &controlplane.RestoreJob{ID: x.job.ID, DatabaseID: x.job.DatabaseID, AgentID: x.job.AgentID, BackupID: x.job.BackupID, SizeBytes: x.job.SizeBytes, Checksum: x.job.Checksum}
			}
			json.NewEncoder(w).Encode(controlplane.RestorePollResponse{Protocol: 1, DatabaseID: x.job.DatabaseID, AgentID: x.job.AgentID, Job: job})
		case strings.HasSuffix(r.URL.Path, "/report"):
			var input controlplane.RestoreReport
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				w.WriteHeader(400)
				return
			}
			// Acceptance must already exist privately before the remote ack.
			observer := x.runner(restorejob.Options{})
			state, err := observer.Snapshot(x.job)
			if err != nil || state.Revision < input.Revision {
				t.Error("progress was reported before durable local state")
			}
			reports = append(reports, input)
			json.NewEncoder(w).Encode(controlplane.RestoreReportResponse{Revision: input.Revision})
			if input.Phase == "succeeded" {
				select {
				case terminal <- struct{}{}:
				default:
				}
			}
		default:
			originalAPI.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	x.settings.APIURL = server.URL
	data, err := yaml.Marshal(x.settings)
	if err != nil || os.WriteFile(x.path, data, 0600) != nil {
		t.Fatal("cannot configure delivery fixture")
	}
	client, err := controlplane.New(server.URL, x.settings.AgentCredential, "0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := restorejob.NewWorker(x.path, client, restorejob.Options{Download: restore.Options{Transport: x.storage.Client().Transport, ReserveBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-terminal:
	case err := <-done:
		t.Fatalf("worker stopped before terminal result: %v", err)
	case <-f.ctx.Done():
		t.Fatal("outbound worker never completed its restore")
	}
	observer := x.runner(restorejob.Options{})
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticks := time.NewTicker(10 * time.Millisecond)
	defer ticks.Stop()
	for {
		state, err := observer.Snapshot(x.job)
		if err == nil && state.Phase == restorejob.Succeeded && !state.Pending() {
			break
		}
		select {
		case <-ticks.C:
		case <-deadline.C:
			t.Fatal("remote exact terminal acknowledgement was not saved")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("delivery worker did not join")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 2 || reports[0].Phase != "accepted" || reports[len(reports)-1].Phase != "succeeded" {
		t.Fatal("worker lost accepted/terminal reporting")
	}
	if f.scalar(f.admin, "SELECT credits FROM "+qi(x.job.Destination())+".delivered_balances WHERE id=1") != 100 {
		t.Fatal("delivered restore is unusable")
	}
	assertExecutionAccountLocked(t, x)
}

type cancellationPlane struct {
	client   *controlplane.Client
	job      restorejob.Job
	cancel   atomic.Bool
	terminal chan controlplane.RestoreReport
}

func (p *cancellationPlane) DownloadBackup(ctx context.Context, id string) (controlplane.DownloadBackupResponse, error) {
	return p.client.DownloadBackup(ctx, id)
}

func (p *cancellationPlane) PollRestores(context.Context, controlplane.RestorePollRequest) (controlplane.RestorePollResponse, error) {
	return controlplane.RestorePollResponse{Protocol: 1, DatabaseID: p.job.DatabaseID, AgentID: p.job.AgentID, Job: &controlplane.RestoreJob{ID: p.job.ID, DatabaseID: p.job.DatabaseID, AgentID: p.job.AgentID, BackupID: p.job.BackupID, SizeBytes: p.job.SizeBytes, Checksum: p.job.Checksum}, CancelRequested: p.cancel.Load()}, nil
}

func (p *cancellationPlane) ReportRestore(_ context.Context, _ string, report controlplane.RestoreReport) (controlplane.RestoreReportResponse, error) {
	if report.Phase == "succeeded" || report.Phase == "failed" || report.Phase == "canceled" {
		select {
		case p.terminal <- report:
		default:
		}
	}
	// Only poll delivers cancellation in this drill. Reports must not be the
	// sole cancellation path while the SQL worker is blocked without progress.
	return controlplane.RestoreReportResponse{Revision: report.Revision}, nil
}

func TestOutboundWorkerPollCancellationJoinsBlockedSQL(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	x := newExecutionFixture(f, f.nativeDump())
	registerExecutionCleanup(x)
	before := f.snapshotSchema(false)
	lockDB := openDB(t, "root", rootPassword)
	locker, err := lockDB.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	defer locker.ExecContext(context.Background(), "UNLOCK TABLES")
	locked := make(chan struct{})
	client, err := controlplane.New(x.settings.APIURL, x.settings.AgentCredential, "0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	plane := &cancellationPlane{client: client, job: x.job, terminal: make(chan controlplane.RestoreReport, 1)}
	worker, err := restorejob.NewWorker(x.path, plane, restorejob.Options{Download: restore.Options{Transport: x.storage.Client().Transport, ReserveBytes: 1 << 20}, Observe: func(state restorejob.State) {
		if state.Phase == restorejob.Validating {
			if _, err := locker.ExecContext(f.ctx, "LOCK TABLES "+qi(x.job.Destination())+".players WRITE"); err != nil {
				t.Error("cannot block validation SQL")
			}
			close(locked)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-locked:
	case <-f.ctx.Done():
		t.Fatal("worker never reached blocked validation")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER=? AND COMMAND='Query' AND LOWER(STATE) LIKE '%lock%'", childUser(x.job)) == 0 {
		select {
		case <-ticker.C:
		case <-f.ctx.Done():
			t.Fatal("SQL did not block")
		}
	}
	plane.cancel.Store(true)
	select {
	case report := <-plane.terminal:
		if report.Phase != "canceled" || !report.WritesStarted {
			t.Fatalf("poll cancellation was not confirmed: %+v", report)
		}
	case <-f.ctx.Done():
		t.Fatal("cancellation poll was blocked by SQL execution")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER=?", childUser(x.job)) != 0 || f.snapshotSchema(false) != before {
		t.Fatal("terminal cancellation left active SQL or changed source")
	}
	assertExecutionAccountLocked(t, x)
}
