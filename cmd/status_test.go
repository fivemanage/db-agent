package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/securefile"
)

func TestBackupStatusShowsRecordedNextRunAndOutcomeWithoutClaimingHealth(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	next := at.Add(time.Hour)
	state := backup.ScheduleState{Version: 1, Expression: "0 4 * * *", RecordedAt: at, NextRun: &next}
	scheduleData, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := securefile.Create(filepath.Join(dir, "backup-transfer", "schedule.json"), scheduleData); err != nil {
		t.Fatal(err)
	}
	run := backup.Snapshot{Version: 1, Activity: controlplane.Error, LastError: controlplane.UploadFailed, Latest: &backup.Run{
		RequestID: strings.Repeat("a", 64), StartedAt: at, Status: "failed", Pending: true,
	}}
	runData, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := securefile.Create(filepath.Join(dir, "backup-transfer", "run.json"), runData); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := printBackupStatus(filepath.Join(dir, "config.yaml"), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Last backup: failed", "pending: true", next.Local().Format(time.RFC3339), "last recorded", "not proof of service health"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("status omitted %q: %s", want, output.String())
		}
	}
}

func TestBackupStatusWithNoRecordedRuns(t *testing.T) {
	var output bytes.Buffer
	if err := printBackupStatus(filepath.Join(t.TempDir(), "config.yaml"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "no recorded run") || !strings.Contains(output.String(), "not recorded") {
		t.Fatal("fresh setup was presented as an existing backup/schedule")
	}
}

func TestBackupStatusDoesNotEchoInvalidPrivateState(t *testing.T) {
	dir := t.TempDir()
	if err := securefile.Create(filepath.Join(dir, "backup-transfer", "schedule.json"), []byte("private-invalid-state")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := printBackupStatus(filepath.Join(dir, "config.yaml"), &output)
	if !errors.Is(err, backup.ErrState) || strings.Contains(err.Error()+output.String(), "private-invalid") {
		t.Fatalf("unsafe status accepted or echoed: %v", err)
	}
}
