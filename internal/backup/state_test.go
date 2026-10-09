package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/securefile"
	"github.com/fivemanage/db-agent/internal/transfer"
)

func TestAbandonedRunIsNotReportedAsActive(t *testing.T) {
	dir := t.TempDir()
	r := testRunner(t, dir, &testPlane{}, nil, transfer.Options{})
	for _, activity := range []controlplane.Status{controlplane.BackingUp, controlplane.Uploading} {
		snapshot := Snapshot{Version: 1, Activity: activity, Latest: &Run{RequestID: strings.Repeat("a", 64), StartedAt: time.Now(), Status: string(activity), Pending: true}}
		if err := r.save(snapshot); err != nil {
			t.Fatal(err)
		}
		state := r.HeartbeatState(controlplane.State{Status: controlplane.Idle})
		if state.Status != controlplane.Error {
			t.Fatalf("abandoned run still reported active: %+v", state)
		}
	}
}

func TestCorruptStateBlocksNewRunsWithoutStartingDump(t *testing.T) {
	dir := t.TempDir()
	r := testRunner(t, dir, &testPlane{}, nil, transfer.Options{})
	if err := securefile.Create(r.statePath(), []byte(`{"version":1,"activity":"idle","last_error":"database-password"}`)); err != nil {
		t.Fatal(err)
	}
	prepared := false
	err := r.Run(context.Background(), func(context.Context) (Plan, error) { prepared = true; return Plan{}, nil })
	if !errors.Is(err, ErrState) || prepared || strings.Contains(err.Error(), "database-password") {
		t.Fatalf("unsafe state wasn't rejected: %v", err)
	}
	if r.HeartbeatState(controlplane.State{Status: controlplane.Idle}).Status != controlplane.Error {
		t.Fatal("unsafe state produced a healthy heartbeat")
	}
}

func TestObserverFailureStopsBeforeDumpAndLeavesRecoverableFailure(t *testing.T) {
	dir := t.TempDir()
	p := &testPlane{}
	r := testRunner(t, dir, p, nil, transfer.Options{})
	staged := r.statePath() + ".next"
	if err := os.Mkdir(staged, 0700); err != nil {
		t.Fatal(err)
	}
	block := filepath.Join(staged, "block")
	if err := os.WriteFile(block, nil, 0600); err != nil {
		t.Fatal(err)
	}
	started := false
	err := r.Run(context.Background(), plan(t, artifact.Zstd, func(context.Context, io.Writer) error { started = true; return nil }))
	if !errors.Is(err, ErrState) || started || len(p.initiations) != 0 {
		t.Fatalf("state publication failure didn't stop the pipeline: %v", err)
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(staged); err != nil {
		t.Fatal(err)
	}
	if err := r.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.completions) != 1 || p.completions[0].Status != "failed" {
		t.Fatal("interrupted run was not recovered as failed")
	}
	assertFiles(t, dir, false, false)
}
