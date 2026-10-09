package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/spf13/cobra"
)

func TestAgentCredentialRejectionDoesNotStartUpdater(t *testing.T) {
	var output bytes.Buffer
	err := runAgent(context.Background(), &output, &output, agentWork{
		heartbeats: func(_ context.Context, report func(controlplane.Result)) error {
			report(controlplane.Result{Connection: controlplane.Rejected, Err: controlplane.ErrCredentialRejected})
			return controlplane.ErrCredentialRejected
		},
		update: func(context.Context) (bool, error) {
			t.Error("updater started with a rejected initial credential")
			return false, nil
		},
		backups: func(context.Context) error {
			t.Error("scheduling started with a rejected initial credential")
			return nil
		},
	}, true)
	if !errors.Is(err, controlplane.ErrCredentialRejected) {
		t.Fatalf("rejection not returned: %v", err)
	}
}

func TestAgentHeartbeatsContinueDuringSlowUpdateAndShutdownJoinsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updateStarted := make(chan struct{})
	secondHeartbeat := make(chan struct{})
	updateStopped := make(chan struct{})
	heartbeatStopped := make(chan struct{})
	done := make(chan error, 1)
	var output bytes.Buffer
	go func() {
		done <- runAgent(ctx, &output, &output, agentWork{
			heartbeats: func(ctx context.Context, report func(controlplane.Result)) error {
				defer close(heartbeatStopped)
				report(controlplane.Result{Connection: controlplane.Connected})
				select {
				case <-updateStarted:
				case <-ctx.Done():
					return nil
				}
				report(controlplane.Result{Connection: controlplane.Connected})
				close(secondHeartbeat)
				<-ctx.Done()
				return nil
			},
			update: func(ctx context.Context) (bool, error) {
				close(updateStarted)
				<-ctx.Done()
				close(updateStopped)
				return false, ctx.Err()
			},
		}, true)
	}()
	select {
	case <-secondHeartbeat:
	case <-time.After(time.Second):
		t.Fatal("update check blocked heartbeats")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not stop")
	}
	select {
	case <-heartbeatStopped:
	default:
		t.Fatal("heartbeat worker leaked")
	}
	select {
	case <-updateStopped:
	default:
		t.Fatal("update worker leaked")
	}
	if strings.Count(output.String(), "Heartbeat accepted.") != 1 {
		t.Fatal("unchanged connection state was repeatedly printed")
	}
}

func TestAgentStagedUpdateStopsHeartbeatBeforeExit(t *testing.T) {
	stopped := make(chan struct{})
	var output bytes.Buffer
	err := runAgent(context.Background(), &output, &output, agentWork{
		heartbeats: func(ctx context.Context, report func(controlplane.Result)) error {
			report(controlplane.Result{Connection: controlplane.Connected})
			<-ctx.Done()
			close(stopped)
			return nil
		},
		update: func(context.Context) (bool, error) { return true, nil },
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("heartbeat did not stop for replacement")
	}
	if !strings.Contains(output.String(), "Update staged") {
		t.Fatal("replacement exit was not reported")
	}
}

func TestAgentWithoutAutoUpdateStillRunsHeartbeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	err := runAgent(ctx, &output, &output, agentWork{
		heartbeats: func(ctx context.Context, report func(controlplane.Result)) error {
			report(controlplane.Result{Connection: controlplane.Connected})
			cancel()
			<-ctx.Done()
			return nil
		},
		update: func(context.Context) (bool, error) { t.Error("disabled updater started"); return false, nil },
	}, false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentSchedulingIndependentOfHeartbeatsAndJoinedForUpdate(t *testing.T) {
	for _, connection := range []controlplane.Connection{controlplane.Connected, controlplane.Unreachable} {
		t.Run(string(connection), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			backupStarted, backupStopped := make(chan struct{}), make(chan struct{})
			heartbeatStopped := make(chan struct{})
			var output bytes.Buffer
			err := runAgent(ctx, &output, &output, agentWork{
				heartbeats: func(ctx context.Context, report func(controlplane.Result)) error {
					defer close(heartbeatStopped)
					report(controlplane.Result{Connection: connection})
					select {
					case <-backupStarted:
					case <-ctx.Done():
						return nil
					}
					report(controlplane.Result{Connection: connection})
					<-ctx.Done()
					return nil
				},
				backups: func(ctx context.Context) error {
					close(backupStarted)
					<-ctx.Done()
					close(backupStopped)
					return nil
				},
				update: func(ctx context.Context) (bool, error) {
					select {
					case <-backupStarted:
						return true, nil
					case <-ctx.Done():
						return false, ctx.Err()
					}
				},
			}, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, stopped := range []chan struct{}{backupStopped, heartbeatStopped} {
				select {
				case <-stopped:
				default:
					t.Fatal("update handoff left a worker running")
				}
			}
		})
	}
}

func TestAgentSchedulerFailureStopsAndJoinsHeartbeats(t *testing.T) {
	stopped := make(chan struct{})
	terminal := errors.New("private state unavailable")
	var output bytes.Buffer
	err := runAgent(context.Background(), &output, &output, agentWork{
		heartbeats: func(ctx context.Context, report func(controlplane.Result)) error {
			report(controlplane.Result{Connection: controlplane.Connected})
			<-ctx.Done()
			close(stopped)
			return nil
		},
		backups: func(context.Context) error { return terminal },
	}, false)
	if !errors.Is(err, terminal) {
		t.Fatalf("scheduler error not returned: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("scheduler failure leaked heartbeat worker")
	}
}

func TestStartupHeartbeatReadinessNeverUsesRawDiagnosticMessages(t *testing.T) {
	state := startupHeartbeatState([]database.Check{{Name: "DB", Message: "private-db-password"}})
	if state != (controlplane.State{Status: controlplane.Error, Problem: controlplane.SetupIncomplete}) {
		t.Fatal("incomplete setup was not classified")
	}
	state = startupHeartbeatState([]database.Check{{Name: "DB", Passed: true}, {Name: "Control-plane reachability", Message: "redirected"}})
	if state.Status != controlplane.Idle {
		t.Fatal("unauthenticated API probe overrode local readiness")
	}
}

func TestForegroundAgentSendsHeartbeatWithIncompleteLocalSetup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const credential = "dac_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	seen := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/heartbeat" {
			io.WriteString(w, `{}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("issued credential not used")
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["status"] != "error" || !strings.Contains(payload["last_error"], "prerequisites") || len(payload) != 3 {
			t.Error("incomplete setup leaked data or falsely reported ready")
		}
		io.WriteString(w, `{}`)
		close(seen)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveEnrollment(path, server.URL, credential); err != nil {
		t.Fatal(err)
	}
	if err := setupRecoveryKey(ctx, path, func(string) error { return nil }, func(bool, key.Key) error { return nil }); err != nil {
		t.Fatal(err)
	}
	previousPath := configPath
	configPath = path
	defer func() { configPath = previousPath }()
	command := &cobra.Command{}
	command.SetContext(ctx)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	go func() {
		select {
		case <-seen:
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := agentCmd.RunE(command, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-seen:
	default:
		t.Fatal("foreground command did not send a heartbeat")
	}
	if strings.Contains(output.String(), credential) {
		t.Fatal("credential leaked in foreground output")
	}
}
