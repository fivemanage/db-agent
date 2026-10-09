package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
)

func TestHeartbeatScheduleOverrideIsSavedAndUsedByScheduler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/heartbeat" {
			t.Error("unexpected control-plane request")
		}
		io.WriteString(w, `{"config_overrides":{"backup":{"schedule":"0 5 * * *"}}}`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveEnrollment(path, server.URL, "dac_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(filepath.Dir(path), "backup-transfer")
	scheduler, err := backup.NewScheduler(dir, settings.Backup.Schedule)
	if err != nil {
		t.Fatal(err)
	}
	client, err := controlplane.New(server.URL, settings.AgentCredential, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.OnOverrides = func(ctx context.Context, overrides config.DashboardOverrides) error {
		return applyAgentOverrides(ctx, path, scheduler, overrides)
	}
	if err := client.Heartbeat(context.Background(), controlplane.State{Status: controlplane.Idle}); err != nil {
		t.Fatal(err)
	}
	settings, err = config.LoadAgent(path)
	if err != nil || settings.Backup.Schedule != "0 5 * * *" {
		t.Fatalf("heartbeat override was not persisted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorded := make(chan *backup.ScheduleState, 1)
	done := make(chan error, 1)
	go func() {
		done <- scheduler.Run(ctx, func(ctx context.Context) error {
			state, err := backup.ReadSchedule(dir)
			if err != nil {
				return err
			}
			recorded <- state
			<-ctx.Done()
			return ctx.Err()
		}, true, nil)
	}()
	select {
	case state := <-recorded:
		if state == nil || state.Expression != "0 5 * * *" || state.NextRun == nil {
			t.Fatal("scheduler kept the pre-heartbeat schedule")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not join")
	}
}
