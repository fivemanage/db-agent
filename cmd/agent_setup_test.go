package cmd

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
)

type fakeSetupPlane struct {
	mu       sync.Mutex
	polls    []controlplane.SetupStatus
	reports  map[string]controlplane.SetupReport
	respond  func(int, controlplane.SetupStatus) (controlplane.SetupPollResponse, error)
	reported chan struct{}
}

func (p *fakeSetupPlane) PollSetup(_ context.Context, status controlplane.SetupStatus) (controlplane.SetupPollResponse, error) {
	p.mu.Lock()
	p.polls = append(p.polls, status)
	count := len(p.polls)
	p.mu.Unlock()
	return p.respond(count, status)
}

func (p *fakeSetupPlane) ReportSetup(_ context.Context, id string, report controlplane.SetupReport) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reports == nil {
		p.reports = map[string]controlplane.SetupReport{}
	}
	p.reports[id] = report
	if p.reported != nil {
		close(p.reported)
		p.reported = nil
	}
	return nil
}

func setupConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveEnrollment(path, "http://127.0.0.1:1", "dac_"+strings.Repeat("A", 43)); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveDatabase(path, config.DatabaseConfig{Driver: "mariadb", Host: "127.0.0.1", Port: 1, User: "db_agent_backup", Password: "secret", Databases: []string{"game"}}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDashboardSetupReportsFailedFinishAndKeepsPolling(t *testing.T) {
	path := setupConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reported := make(chan struct{})
	plane := &fakeSetupPlane{reported: reported, respond: func(count int, _ controlplane.SetupStatus) (controlplane.SetupPollResponse, error) {
		response := controlplane.SetupPollResponse{Protocol: 1}
		if count == 1 {
			response.Request = &controlplane.SetupRequest{ID: "request", Kind: controlplane.SetupFinish}
		}
		return response, nil
	}}
	done := make(chan error, 1)

	go func() {
		done <- runDashboardSetup(ctx, path, plane, time.Millisecond, func(context.Context) { t.Error("failed finish completed setup") }, io.Discard)
	}()
	<-reported
	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if report := plane.reports["request"]; report.Succeeded || len(report.Failed) == 0 {
		t.Fatalf("finish result was not reported as failed: %+v", report)
	}
	if len(plane.polls) < 3 {
		t.Fatal("setup stopped polling after a failed finish")
	}
	settings, err := config.LoadDiagnostic(path)
	if err != nil || settings.Encryption.KeyAcknowledged {
		t.Fatal("failed finish acknowledged the recovery key")
	}
}

func TestDashboardSetupRefreshesReadinessOnceWhenSetupCompletes(t *testing.T) {
	path := setupConfig(t)
	finished := 0
	plane := &fakeSetupPlane{respond: func(count int, status controlplane.SetupStatus) (controlplane.SetupPollResponse, error) {
		if count == 2 {
			// Simulate a completed acknowledgement, for example from db-agent configure --key.
			settings, err := config.LoadDiagnostic(path)
			if err != nil {
				return controlplane.SetupPollResponse{}, err
			}
			if err := config.SaveKeyBinding(path, status.KeyFile, settings.Encryption.KeyID, true); err != nil {
				return controlplane.SetupPollResponse{}, err
			}
		}
		return controlplane.SetupPollResponse{Protocol: 1}, nil
	}}

	err := runDashboardSetup(context.Background(), path, plane, time.Millisecond, func(context.Context) { finished++ }, io.Discard)

	if err != nil || finished != 1 {
		t.Fatalf("completion did not refresh readiness exactly once: finished=%d err=%v", finished, err)
	}
	if last := plane.polls[len(plane.polls)-1]; !last.Complete {
		t.Fatal("completed setup was not reported to the dashboard")
	}
}

func TestDashboardSetupStopsQuietlyWhenBackendIsUnsupported(t *testing.T) {
	path := setupConfig(t)
	plane := &fakeSetupPlane{respond: func(int, controlplane.SetupStatus) (controlplane.SetupPollResponse, error) {
		return controlplane.SetupPollResponse{}, &controlplane.ResponseError{StatusCode: http.StatusNotFound}
	}}

	err := runDashboardSetup(context.Background(), path, plane, time.Millisecond, func(context.Context) { t.Error("unsupported backend completed setup") }, io.Discard)

	if err != nil || len(plane.polls) != 1 {
		t.Fatalf("unsupported setup endpoint was not treated as optional: %v", err)
	}
}
