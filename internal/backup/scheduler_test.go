package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/securefile"
	"github.com/fivemanage/db-agent/internal/transfer"
)

type scheduleClock struct {
	mu      sync.Mutex
	current time.Time
	sampled chan time.Time
}

func (c *scheduleClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sampled <- c.current
	return c.current
}

type scheduleHarness struct {
	scheduler *Scheduler
	clock     *scheduleClock
	ticks     chan time.Time
	recorded  chan ScheduleState
	reports   chan error
	done      chan error
	cancel    context.CancelFunc
}

func startSchedule(t *testing.T, dir, expression string, at time.Time, startup bool, trigger func(context.Context) error) *scheduleHarness {
	t.Helper()
	scheduler, err := NewScheduler(dir, expression)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("scheduler leaked after test")
		}
	})
	h := &scheduleHarness{
		scheduler: scheduler, clock: &scheduleClock{current: at, sampled: make(chan time.Time, 32)}, ticks: make(chan time.Time),
		recorded: make(chan ScheduleState, 32), reports: make(chan error, 32), done: make(chan error, 1), cancel: cancel,
	}
	go func() {
		defer close(finished)
		h.done <- scheduler.run(ctx, trigger, startup, func(err error) { h.reports <- err }, h.clock.now, h.ticks, func(state ScheduleState) error {
			if err := scheduler.save(state); err != nil {
				return err
			}
			if state.NextRun != nil {
				next := *state.NextRun
				state.NextRun = &next
			}
			h.recorded <- state
			return nil
		})
	}()
	return h
}

func (h *scheduleHarness) step(t *testing.T, at time.Time) {
	t.Helper()
	h.clock.mu.Lock()
	h.clock.current = at
	h.clock.mu.Unlock()
	select {
	case h.ticks <- at:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not sample clock")
	}
	for {
		select {
		case sampled := <-h.clock.sampled:
			if sampled.Equal(at) {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("clock sample did not finish")
		}
	}
}

func (h *scheduleHarness) next(t *testing.T, want time.Time) ScheduleState {
	t.Helper()
	select {
	case state := <-h.recorded:
		if state.NextRun == nil || !state.NextRun.Equal(want) {
			t.Fatalf("next run = %v, want %v", state.NextRun, want)
		}
		return state
	case <-time.After(5 * time.Second):
		t.Fatal("next run was not recorded")
		return ScheduleState{}
	}
}

func (h *scheduleHarness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	if err := awaitResult(t, h.done); err != nil {
		t.Fatal(err)
	}
	state, err := ReadSchedule(h.scheduler.dir)
	if err != nil || state == nil || state.NextRun != nil {
		t.Fatalf("stopped scheduler still advertises a run: %+v %v", state, err)
	}
}

func localDate(t *testing.T, zone, value string) time.Time {
	t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	date, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return date.In(location)
}

func TestScheduleLocalTimeAndDST(t *testing.T) {
	for _, test := range []struct {
		name, zone, expression, start, next string
	}{
		{"UTC", "UTC", "0 4 * * *", "2026-10-09T03:59:00Z", "2026-10-09T04:00:00Z"},
		{"offset", "Asia/Kathmandu", "0 4 * * *", "2026-10-09T03:59:00+05:45", "2026-10-09T04:00:00+05:45"},
		{"at occurrence", "Europe/Oslo", "0 4 * * *", "2026-10-09T04:00:00+02:00", "2026-10-10T04:00:00+02:00"},
		{"spring 23 hour day", "America/New_York", "0 4 * * *", "2026-03-07T04:00:00-05:00", "2026-03-08T04:00:00-04:00"},
		{"fall 25 hour day", "America/New_York", "0 4 * * *", "2026-10-31T04:00:00-04:00", "2026-11-01T04:00:00-05:00"},
		{"missing hour skipped", "America/New_York", "30 2 * * *", "2026-03-08T00:00:00-05:00", "2026-03-09T02:30:00-04:00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startSchedule(t, t.TempDir(), test.expression, localDate(t, test.zone, test.start), false, func(context.Context) error {
				t.Error("restart replayed a missed or current occurrence")
				return nil
			})
			h.next(t, localDate(t, test.zone, test.next))
			h.stop(t)
		})
	}
}

func TestScheduleRepeatedDSTHourUsesDistinctInstants(t *testing.T) {
	var runs atomic.Int32
	h := startSchedule(t, t.TempDir(), "30 1 * * *", localDate(t, "America/New_York", "2026-11-01T00:00:00-04:00"), false, func(context.Context) error {
		runs.Add(1)
		return nil
	})
	first := localDate(t, "America/New_York", "2026-11-01T01:30:00-04:00")
	second := localDate(t, "America/New_York", "2026-11-01T01:30:00-05:00")
	h.next(t, first)
	h.step(t, first)
	h.next(t, second)
	if err := awaitResult(t, h.reports); err != nil {
		t.Fatal(err)
	}
	h.step(t, second)
	h.next(t, localDate(t, "America/New_York", "2026-11-02T01:30:00-05:00"))
	if err := awaitResult(t, h.reports); err != nil {
		t.Fatal(err)
	}
	h.stop(t)
	if runs.Load() != 2 {
		t.Fatal("repeated local hour did not follow robfig calendar semantics")
	}
}

func TestScheduleClockCorrectionsDoNotReplayOrBurst(t *testing.T) {
	var runs atomic.Int32
	at := time.Date(2026, 10, 9, 3, 59, 0, 0, time.UTC)
	h := startSchedule(t, t.TempDir(), "0 4 * * *", at, false, func(context.Context) error {
		runs.Add(1)
		return nil
	})
	first := at.Add(time.Minute)
	h.next(t, first)
	h.step(t, first)
	h.next(t, first.AddDate(0, 0, 1))
	if err := awaitResult(t, h.reports); err != nil {
		t.Fatal(err)
	}

	h.step(t, first.Add(-time.Hour))
	h.next(t, first.AddDate(0, 0, 1))
	h.step(t, first)
	jump := first.AddDate(0, 0, 3).Add(time.Hour)
	h.step(t, jump)
	h.next(t, first.AddDate(0, 0, 4))
	if err := awaitResult(t, h.reports); err != nil {
		t.Fatal(err)
	}
	h.stop(t)
	if runs.Load() != 2 {
		t.Fatalf("clock correction replayed/burst backup runs: %d", runs.Load())
	}
}

func TestScheduleOverrideAndRestartReadSavedPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := config.SaveEnrollment(path, "https://api.example.com", "dac_local"); err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	h := startSchedule(t, dir, settings.Backup.Schedule, at, false, func(context.Context) error {
		t.Error("policy update/restart ran a backup immediately")
		return nil
	})
	h.next(t, at.Add(time.Hour))
	expression := "0 5 * * *"
	if err := config.SaveDashboardOverrides(path, config.DashboardOverrides{Backup: config.BackupOverrides{Schedule: &expression}}); err != nil {
		t.Fatal(err)
	}
	if err := h.scheduler.Update(expression); err != nil {
		t.Fatal(err)
	}
	h.step(t, at.Add(time.Minute))
	if state := h.next(t, at.Add(2*time.Hour)); state.Expression != expression {
		t.Fatal("override not recorded")
	}
	if err := h.scheduler.Update("private-invalid-schedule"); err == nil || strings.Contains(err.Error(), "private-invalid") {
		t.Fatal("invalid update accepted or echoed")
	}
	h.stop(t)

	settings, err = config.LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := startSchedule(t, dir, settings.Backup.Schedule, at.Add(3*time.Hour), false, func(context.Context) error {
		t.Error("restart replayed old next run")
		return nil
	})
	restarted.next(t, at.AddDate(0, 0, 1).Add(2*time.Hour))
	restarted.stop(t)
}

func TestScheduleRestartHonorsLocalEditAndTimezone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := config.SaveEnrollment(path, "https://api.example.com", "dac_local"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	h := startSchedule(t, dir, "0 4 * * *", at, false, func(context.Context) error { return nil })
	h.next(t, at.Add(time.Hour))
	h.stop(t)
	if err := os.WriteFile(path, []byte("agent_credential: dac_local\nbackup:\n  schedule: '0 7 * * *'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	local := localDate(t, "Europe/Oslo", "2026-10-09T06:00:00+02:00")
	restarted := startSchedule(t, dir, settings.Backup.Schedule, local, false, func(context.Context) error {
		t.Error("restart replayed the prior plan")
		return nil
	})
	restarted.next(t, local.Add(time.Hour))
	restarted.stop(t)
}

func TestStartupScheduledAndExternalTriggersShareWholeRunLock(t *testing.T) {
	dir := t.TempDir()
	runner := testRunner(t, dir, &testPlane{}, nil, transfer.Options{})
	external := testRunner(t, dir, &testPlane{}, nil, transfer.Options{})
	started, joined := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	prepare := plan(t, artifact.Gzip, func(ctx context.Context, _ io.Writer) error {
		close(started)
		defer close(joined)
		<-ctx.Done()
		return ctx.Err()
	})
	at := time.Date(2026, 10, 9, 3, 59, 0, 0, time.UTC)
	h := startSchedule(t, dir, "0 4 * * *", at, true, func(ctx context.Context) error {
		runs.Add(1)
		return runner.Run(ctx, prepare)
	})
	h.next(t, at.Add(time.Minute))
	await(t, started)
	for _, trigger := range []string{"manual", "remote-style"} {
		t.Run(trigger, func(t *testing.T) {
			if err := external.Run(context.Background(), prepare); !errors.Is(err, ErrRunning) {
				t.Fatalf("external trigger overlapped startup: %v", err)
			}
		})
	}
	if err := external.Resume(context.Background()); !errors.Is(err, ErrRunning) {
		t.Fatalf("recovery overlapped startup: %v", err)
	}
	h.step(t, at.Add(time.Minute))
	h.next(t, at.AddDate(0, 0, 1).Add(time.Minute))
	h.step(t, at.AddDate(0, 0, 1).Add(time.Minute))
	h.next(t, at.AddDate(0, 0, 2).Add(time.Minute))
	h.stop(t)
	await(t, joined)
	if runs.Load() != 1 {
		t.Fatal("busy ticks were queued or ran concurrently")
	}
}

func TestScheduleCredentialRejectionIsTerminal(t *testing.T) {
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	h := startSchedule(t, t.TempDir(), "0 4 * * *", at, true, func(context.Context) error {
		return controlplane.ErrCredentialRejected
	})
	h.next(t, at.Add(time.Hour))
	if err := awaitResult(t, h.done); !errors.Is(err, controlplane.ErrCredentialRejected) {
		t.Fatalf("credential rejection wasn't terminal: %v", err)
	}
	state, err := ReadSchedule(h.scheduler.dir)
	if err != nil || state.NextRun != nil {
		t.Fatal("terminal failure left a live schedule")
	}
}

func TestScheduleRunFailureDoesNotDisableNextOccurrence(t *testing.T) {
	var runs atomic.Int32
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	h := startSchedule(t, t.TempDir(), "0 4 * * *", at, true, func(context.Context) error {
		if runs.Add(1) == 1 {
			return ErrQueued
		}
		return nil
	})
	h.next(t, at.Add(time.Hour))
	if err := awaitResult(t, h.reports); !errors.Is(err, ErrQueued) {
		t.Fatal("queued backup not reported")
	}
	h.step(t, at.Add(time.Hour))
	h.next(t, at.AddDate(0, 0, 1).Add(time.Hour))
	if err := awaitResult(t, h.reports); err != nil {
		t.Fatal(err)
	}
	h.stop(t)
	if runs.Load() != 2 {
		t.Fatal("recoverable failure disabled or immediately retried scheduling")
	}
}

func TestOnlyOneSchedulerOwnsDirectory(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	h := startSchedule(t, dir, "0 4 * * *", at, false, func(context.Context) error { return nil })
	h.next(t, at.Add(time.Hour))
	other, err := NewScheduler(dir, "0 5 * * *")
	if err != nil {
		t.Fatal(err)
	}
	err = other.Run(context.Background(), func(context.Context) error {
		t.Error("duplicate scheduler started a backup")
		return nil
	}, true, nil)
	if !errors.Is(err, ErrSchedulerRunning) {
		t.Fatalf("duplicate scheduling was allowed: %v", err)
	}
	h.stop(t)

	restarted := startSchedule(t, dir, "0 5 * * *", at, false, func(context.Context) error { return nil })
	restarted.next(t, at.Add(2*time.Hour))
	restarted.stop(t)
}

func TestScheduleStateFailurePreventsTrigger(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "schedule.json.next")
	if err := os.Mkdir(staged, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "block"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	h := startSchedule(t, dir, "0 4 * * *", at, true, func(context.Context) error {
		t.Error("backup started without recorded schedule")
		return nil
	})
	if err := awaitResult(t, h.done); !errors.Is(err, ErrState) {
		t.Fatalf("state failure not returned: %v", err)
	}
}

func TestReadScheduleRejectsInvalidStateWithoutEchoing(t *testing.T) {
	for _, data := range []string{
		`{"version":1,"expression":"private-invalid","recorded_at":"2026-10-09T03:00:00Z"}`,
		`{"version":2,"expression":"0 4 * * *","recorded_at":"2026-10-09T03:00:00Z"}`,
		`{"version":1,"expression":"0 4 * * *","recorded_at":"2026-10-09T03:00:00Z","next_run":"2026-10-09T02:00:00Z"}`,
		`private-invalid-json`,
	} {
		dir := t.TempDir()
		if err := securefile.Create(filepath.Join(dir, "schedule.json"), []byte(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSchedule(dir); !errors.Is(err, ErrState) || strings.Contains(err.Error(), "private-invalid") {
			t.Fatalf("unsafe state accepted/echoed: %v", err)
		}
	}
	if state, err := ReadSchedule(t.TempDir()); err != nil || state != nil {
		t.Fatal("fresh install should have no recorded schedule")
	}
}
