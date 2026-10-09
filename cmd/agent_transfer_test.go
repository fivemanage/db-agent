package cmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/transfer"
)

func TestTransferReplayDoesNotBlockHeartbeatAndJoinsOnStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	polledAgain := make(chan struct{})
	joined := make(chan struct{})
	done := make(chan error, 1)
	var reports atomic.Int32

	heartbeats := func(ctx context.Context, report func(controlplane.Result)) error {
		report(controlplane.Result{Connection: controlplane.Connected})
		select {
		case <-started:
		case <-ctx.Done():
			return nil
		}

		report(controlplane.Result{Connection: controlplane.Connected})
		close(polledAgain)
		<-ctx.Done()

		return nil
	}
	resume := func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(joined)

		return ctx.Err()
	}
	report := func(controlplane.Result) {
		reports.Add(1)
	}

	go func() {
		done <- runHeartbeatTransfers(ctx, heartbeats, resume, report, io.Discard)
	}()

	select {
	case <-polledAgain:
	case <-time.After(time.Second):
		t.Fatal("transfer blocked heartbeat")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("workers didn't stop")
	}

	select {
	case <-joined:
	default:
		t.Fatal("transfer worker wasn't joined")
	}
	if reports.Load() != 2 {
		t.Fatal("heartbeat reports weren't forwarded")
	}
}

type queuedReportPlane struct {
	fail      bool
	completed int
}

func (*queuedReportPlane) InitiateBackup(context.Context, controlplane.InitiateBackupRequest) (controlplane.InitiateBackupResponse, error) {
	return controlplane.InitiateBackupResponse{
		BackupID:  "backup_1",
		UploadURL: "https://storage.example/backup",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

func (p *queuedReportPlane) CompleteBackup(_ context.Context, _ string, input controlplane.CompleteBackupRequest) error {
	if input.Status != "failed" || input.Problem != controlplane.BackupFailed {
		return errors.New("wrong queued failure")
	}
	if p.fail {
		return &controlplane.ResponseError{StatusCode: 503}
	}

	p.completed++

	return nil
}

func TestQueuedCompletionSentOnlyAfterSuccessfulHeartbeat(t *testing.T) {
	p := &queuedReportPlane{fail: true}
	u, err := transfer.New(t.TempDir(), p, transfer.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Invalid key gives a classified failed report without sending any raw dump.
	source := io.NopCloser(strings.NewReader("private dump"))
	err = u.Run(context.Background(), source, key.Key{}, artifact.Zstd, nil)
	if !errors.Is(err, transfer.ErrPending) {
		t.Fatalf("failure wasn't queued: %v", err)
	}

	p.fail = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	afterFailedPoll := make(chan struct{})
	allowSuccess := make(chan struct{})
	replayed := make(chan struct{})
	done := make(chan error, 1)

	heartbeats := func(ctx context.Context, report func(controlplane.Result)) error {
		report(controlplane.Result{Connection: controlplane.Unreachable})
		close(afterFailedPoll)
		select {
		case <-allowSuccess:
		case <-ctx.Done():
			return nil
		}

		report(controlplane.Result{Connection: controlplane.Connected})
		<-ctx.Done()

		return nil
	}
	resume := func(ctx context.Context) error {
		err := u.Resume(ctx)
		close(replayed)

		return err
	}

	go func() {
		done <- runHeartbeatTransfers(ctx, heartbeats, resume, nil, io.Discard)
	}()

	<-afterFailedPoll
	select {
	case <-replayed:
		t.Fatal("failed heartbeat triggered replay")
	default:
	}

	close(allowSuccess)
	select {
	case <-replayed:
	case <-time.After(time.Second):
		t.Fatal("successful heartbeat didn't replay report")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if p.completed != 1 {
		t.Fatal("queued completion wasn't delivered")
	}
}

func TestRejectedTransferCredentialStopsHeartbeatWorkers(t *testing.T) {
	heartbeats := func(ctx context.Context, report func(controlplane.Result)) error {
		report(controlplane.Result{Connection: controlplane.Connected})
		<-ctx.Done()

		return nil
	}
	resume := func(context.Context) error {
		return controlplane.ErrCredentialRejected
	}

	err := runHeartbeatTransfers(context.Background(), heartbeats, resume, nil, io.Discard)

	if !errors.Is(err, controlplane.ErrCredentialRejected) {
		t.Fatalf("credential rejection wasn't terminal: %v", err)
	}
}
