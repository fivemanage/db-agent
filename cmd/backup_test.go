package cmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/transfer"
)

type manualHeartbeat struct {
	run   func(context.Context, func() controlplane.State) error
	final func(controlplane.State) error
}

func (h manualHeartbeat) RunHeartbeat(ctx context.Context, current func() controlplane.State, _ func(controlplane.Result)) error {
	return h.run(ctx, current)
}

func (h manualHeartbeat) Heartbeat(_ context.Context, state controlplane.State) error {
	return h.final(state)
}

func manualRunner(t *testing.T) *backup.Runner {
	t.Helper()
	dir := t.TempDir()
	u, err := transfer.New(dir, &queuedReportPlane{}, transfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := backup.New(dir, u)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBackupCommandRequiresExplicitNow(t *testing.T) {
	command := newBackupCommand()
	command.SetArgs(nil)
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "--now") {
		t.Fatalf("manual trigger didn't require --now: %v", err)
	}
}

func TestManualBackupHeartbeatSeesDumpAndFinalErrorAfterJoining(t *testing.T) {
	r := manualRunner(t)
	started, sampled, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finalState controlplane.State
	heartbeats := manualHeartbeat{
		run: func(ctx context.Context, current func() controlplane.State) error {
			<-started
			if state := current(); state.Status != controlplane.BackingUp {
				t.Errorf("wrong in-flight state: %+v", state)
			}
			close(sampled)
			<-ctx.Done()
			close(joined)
			return nil
		},
		final: func(state controlplane.State) error {
			select {
			case <-joined:
			default:
				t.Error("terminal heartbeat raced the periodic worker")
			}
			finalState = state
			return &controlplane.TransportError{}
		},
	}
	k, err := key.Parse("dbak1_" + strings.Repeat("A", 42) + "E")
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(context.Context) (backup.Plan, error) {
		return backup.Plan{Key: k, Compression: artifact.Zstd, Dump: func(_ context.Context, dst io.Writer) error {
			close(started)
			<-sampled
			return errors.New("private SQL password")
		}}, nil
	}
	err = runManualBackup(context.Background(), r, heartbeats, prepare)
	if !errors.Is(err, backup.ErrFailed) || strings.Contains(err.Error(), "password") || finalState.Status != controlplane.Error || finalState.Problem != controlplane.BackupFailed {
		t.Fatalf("bad terminal error/heartbeat: %v %+v", err, finalState)
	}
}

func TestManualBackupCredentialRejectionCancelsAndJoinsDump(t *testing.T) {
	r := manualRunner(t)
	started, joined := make(chan struct{}), make(chan struct{})
	heartbeats := manualHeartbeat{
		run: func(context.Context, func() controlplane.State) error {
			<-started
			return controlplane.ErrCredentialRejected
		},
		final: func(controlplane.State) error { t.Error("rejected credential sent another heartbeat"); return nil },
	}
	k, err := key.Parse("dbak1_" + strings.Repeat("A", 42) + "E")
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(context.Context) (backup.Plan, error) {
		return backup.Plan{Key: k, Compression: artifact.Gzip, Dump: func(ctx context.Context, _ io.Writer) error {
			defer close(joined)
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	done := make(chan error, 1)
	go func() { done <- runManualBackup(context.Background(), r, heartbeats, prepare) }()
	select {
	case err := <-done:
		if !errors.Is(err, controlplane.ErrCredentialRejected) {
			t.Fatalf("credential rejection wasn't terminal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rejection didn't join workers")
	}
	select {
	case <-joined:
	default:
		t.Fatal("dump was not joined")
	}
}
