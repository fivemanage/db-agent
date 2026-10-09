package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/securefile"
	"github.com/fivemanage/db-agent/internal/transfer"
	"github.com/robfig/cron/v3"
)

var ErrSchedulerRunning = errors.New("backup scheduling is already active; stop the other agent using this config directory")

type ScheduleState struct {
	Version    int        `json:"version"`
	Expression string     `json:"expression"`
	RecordedAt time.Time  `json:"recorded_at"`
	NextRun    *time.Time `json:"next_run,omitempty"`
}

type Scheduler struct {
	dir        string
	mu         sync.Mutex
	expression string
	schedule   cron.Schedule
}

func NewScheduler(dir, expression string) (*Scheduler, error) {
	if dir == "" {
		return nil, ErrState
	}
	s := &Scheduler{dir: dir}
	if err := s.Update(expression); err != nil {
		return nil, err
	}
	return s, nil
}

// Update affects the next clock sample, not an already-running backup.
func (s *Scheduler) Update(expression string) error {
	parsed, err := (config.BackupConfig{Schedule: expression}).ParseSchedule()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expression, s.schedule = expression, parsed
	return nil
}

func (s *Scheduler) policy() (string, cron.Schedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expression, s.schedule
}

// Run owns scheduling, but every trigger must still enter Runner.Run for the
// cross-process whole-run lock. Busy ticks are skipped rather than queued.
func (s *Scheduler) Run(ctx context.Context, trigger func(context.Context) error, startup bool, report func(error)) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	return s.run(ctx, trigger, startup, report, time.Now, ticker.C, s.save)
}

func (s *Scheduler) run(parent context.Context, trigger func(context.Context) error, startup bool, report func(error), now func() time.Time, ticks <-chan time.Time, record func(ScheduleState) error) (result error) {
	if parent.Err() != nil {
		return nil
	}
	if trigger == nil {
		return errors.New("backup scheduling requires a whole-run trigger")
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return &safeError{ErrState, err}
	}
	unlock, err := securefile.TryLock(filepath.Join(s.dir, ".schedule.lock"))
	if errors.Is(err, securefile.ErrLocked) {
		return ErrSchedulerRunning
	}
	if err != nil {
		return &safeError{ErrState, err}
	}
	defer unlock()

	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	state := ScheduleState{Version: 1}
	defer func() {
		cancel()
		workers.Wait()
		state.NextRun, state.RecordedAt = nil, now().Round(0)
		result = errors.Join(result, record(state))
	}()

	// Wall-clock sampling, rather than one long monotonic timer, detects clock
	// corrections. robfig determines calendar/DST occurrences in local time.
	last := now().Round(0)
	expression, parsed := s.policy()
	next := parsed.Next(last)
	state.Expression, state.RecordedAt, state.NextRun = expression, last, &next
	if next.IsZero() {
		return errors.New("backup schedule has no upcoming run")
	}
	if err := record(state); err != nil {
		return err
	}

	done := make(chan error, 1)
	running := false
	start := func() {
		if running || ctx.Err() != nil {
			return
		}
		running = true
		workers.Add(1)
		go func() {
			defer workers.Done()
			done <- trigger(ctx)
		}()
	}
	if startup {
		start()
	}
	var lastTriggered time.Time

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			running = false
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, controlplane.ErrCredentialRejected) || errors.Is(err, ErrState) || errors.Is(err, transfer.ErrLocal) {
				return err
			}
			if report != nil {
				report(err)
			}
		case _, ok := <-ticks:
			if !ok || ctx.Err() != nil {
				return nil
			}
			current := now().Round(0)
			updated, policy := s.policy()
			changed := updated != expression || current.Before(last)
			due := !changed && !current.Before(next)
			if changed || due {
				expression, parsed = updated, policy
				base := current
				// Don't replay an already-consumed instant after a backward jump.
				if base.Before(lastTriggered) {
					base = lastTriggered
				}
				next = parsed.Next(base)
				if next.IsZero() {
					return errors.New("backup schedule has no upcoming run")
				}
				state.Expression, state.RecordedAt, state.NextRun = expression, current, &next
				if err := record(state); err != nil {
					return err
				}
				if due {
					lastTriggered = current
					start()
				}
			}
			last = current
		}
	}
}

// ReadSchedule exposes the last recorded plan, not proof that a service is alive.
// A graceful stop clears NextRun; a crash may leave a stale timestamp.
func ReadSchedule(dir string) (*ScheduleState, error) {
	var data []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		data, err = securefile.Read(filepath.Join(dir, "schedule.json"), 4096)
		if !errors.Is(err, securefile.ErrUnsafe) {
			break
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &safeError{ErrState, err}
	}
	var state ScheduleState
	if json.Unmarshal(data, &state) != nil || !state.valid() {
		return nil, ErrState
	}
	return &state, nil
}

func (s ScheduleState) valid() bool {
	if s.Version != 1 || s.RecordedAt.IsZero() || s.NextRun != nil && !s.NextRun.After(s.RecordedAt) {
		return false
	}
	_, err := (config.BackupConfig{Schedule: s.Expression}).ParseSchedule()
	return err == nil
}

func (s *Scheduler) save(state ScheduleState) error {
	if !state.valid() {
		return ErrState
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > 4096 {
		return ErrState
	}
	path := filepath.Join(s.dir, "schedule.json")
	staged := path + ".next"
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &safeError{ErrState, err}
	}
	if err := securefile.Create(staged, data); err != nil {
		return &safeError{ErrState, err}
	}
	defer os.Remove(staged)
	if err := securefile.ReplaceDurable(staged, path); err != nil {
		return &safeError{ErrState, err}
	}
	return nil
}
