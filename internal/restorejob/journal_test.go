package restorejob

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/operation"
	"github.com/fivemanage/db-agent/internal/securefile"
)

func testJob() Job {
	return Job{
		ID: "restore_1", Binding: Binding{DatabaseID: "database_1", AgentID: "agent_1"},
		BackupID: "backup_1", SizeBytes: 512, Checksum: "sha256:" + strings.Repeat("a", 64),
	}
}

func newJournal(t *testing.T) *Journal {
	t.Helper()
	root := t.TempDir()
	j, err := New(filepath.Join(root, "restore-jobs"), filepath.Join(root, "backup-transfer"), testJob().Binding)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func openJob(t *testing.T, j *Journal, job Job) *Session {
	t.Helper()
	s, err := j.Open(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func reach(t *testing.T, s *Session, phase Phase) {
	t.Helper()
	ctx := context.Background()
	steps := []struct {
		phase Phase
		step  func() error
	}{
		{Accepted, func() error { return nil }},
		{Downloading, func() error { return s.StartDownload(ctx) }},
		{Verified, func() error {
			job := s.Snapshot().Job
			return s.MarkVerified(ctx, artifact.Metadata{SizeBytes: job.SizeBytes, Checksum: job.Checksum})
		}},
		{Importing, func() error { return s.BeginImport(ctx) }},
		{Validating, func() error { return s.BeginValidation(ctx) }},
		{Succeeded, func() error { return s.Succeed(ctx) }},
	}
	for _, step := range steps {
		if err := step.step(); err != nil {
			t.Fatal(err)
		}
		if step.phase == phase {
			return
		}
	}
	t.Fatalf("unknown test phase %s", phase)
}

func assertSaved(t *testing.T, s *Session) State {
	t.Helper()
	want := s.Snapshot()
	got, err := load(s.path)
	if err != nil || got != want {
		t.Fatalf("memory differs from durable state: got %+v, want %+v, err %v", got, want, err)
	}
	return got
}

func TestLifecyclePersistsBeforeAcknowledgementAndTerminalReplay(t *testing.T) {
	j := newJournal(t)
	s := openJob(t, j, testJob())
	accepted := assertSaved(t, s)
	if accepted.Phase != Accepted || accepted.Revision != 1 || !accepted.Pending() || accepted.WritesStarted {
		t.Fatalf("wrong accepted state: %+v", accepted)
	}
	if err := s.Acknowledge(accepted.Revision); err != nil {
		t.Fatal(err)
	}
	if assertSaved(t, s).Pending() {
		t.Fatal("acknowledged acceptance is still pending")
	}

	reach(t, s, Succeeded)
	terminal := assertSaved(t, s)
	if !terminal.Terminal() || !terminal.Pending() || !terminal.WritesStarted || terminal.Revision != 6 {
		t.Fatalf("wrong success state: %+v", terminal)
	}
	if err := s.Succeed(context.Background()); err != nil || s.Snapshot() != terminal {
		t.Fatalf("identical success was not idempotent: %v", err)
	}
	s.Close()

	// A later restore must not evict the first job's deduplication record.
	other := testJob()
	other.ID = "restore_2"
	second := openJob(t, j, other)
	if err := second.Cancel(); err != nil {
		t.Fatal(err)
	}
	second.Close()

	restarted, err := New(j.dir, j.operationDir, j.binding)
	if err != nil {
		t.Fatal(err)
	}
	replay := openJob(t, restarted, testJob())
	if replay.Snapshot() != terminal {
		t.Fatal("restart or duplicate delivery changed the terminal result")
	}
	for _, step := range []func() error{
		func() error { return replay.StartDownload(context.Background()) },
		func() error { return replay.BeginImport(context.Background()) },
		func() error { return replay.Fail(ImportFailed) },
		replay.Cancel,
	} {
		if err := step(); !errors.Is(err, ErrTransition) || replay.Snapshot() != terminal {
			t.Fatalf("terminal result was overwritten: %v", err)
		}
	}
	if err := replay.Acknowledge(terminal.Revision); err != nil {
		t.Fatal(err)
	}
	if assertSaved(t, replay).Pending() {
		t.Fatal("terminal result not durably acknowledged")
	}
}

func TestRecoveryNeverRepeatsAnImport(t *testing.T) {
	for _, phase := range []Phase{Accepted, Downloading, Verified, Importing, Validating, Succeeded} {
		t.Run(string(phase), func(t *testing.T) {
			j := newJournal(t)
			s := openJob(t, j, testJob())
			reach(t, s, phase)
			before := assertSaved(t, s)
			s.Close()

			recovered := openJob(t, j, testJob())
			state := assertSaved(t, recovered)
			switch phase {
			case Accepted, Succeeded:
				if state != before {
					t.Fatal("stable state changed on replay")
				}
			case Downloading, Verified:
				if state.Phase != Accepted || state.WritesStarted || state.Revision != before.Revision+1 {
					t.Fatalf("pre-import recovery did not require re-verification: %+v", state)
				}
				if err := recovered.BeginImport(context.Background()); !errors.Is(err, ErrTransition) {
					t.Fatalf("recovery trusted stale SQL: %v", err)
				}
			case Importing, Validating:
				if state.Phase != Failed || state.Problem != ImportInterrupted || !state.WritesStarted || !state.Pending() || state.Revision != before.Revision+1 {
					t.Fatalf("interrupted import was not made terminal: %+v", state)
				}
				if err := recovered.BeginImport(context.Background()); !errors.Is(err, ErrTransition) {
					t.Fatalf("recovery repeated an import: %v", err)
				}
				recovered.Close()
				again := openJob(t, j, testJob())
				if again.Snapshot() != state {
					t.Fatal("repeated recovery changed the interrupted outcome")
				}
			}
		})
	}
}

func TestIdentityAndBindingCannotChangeOnRedelivery(t *testing.T) {
	for _, field := range []string{"database", "agent", "backup", "size", "checksum"} {
		t.Run(field, func(t *testing.T) {
			j := newJournal(t)
			s := openJob(t, j, testJob())
			original := assertSaved(t, s)
			s.Close()
			changed := testJob()
			want := ErrConflict
			switch field {
			case "database":
				changed.DatabaseID = "database_2"
				want = ErrBinding
			case "agent":
				changed.AgentID = "agent_2"
				want = ErrBinding
			case "backup":
				changed.BackupID = "backup_2"
			case "size":
				changed.SizeBytes++
			case "checksum":
				changed.Checksum = "sha256:" + strings.Repeat("b", 64)
			}
			if next, err := j.Open(context.Background(), changed); !errors.Is(err, want) || next != nil {
				t.Fatalf("changed binding or identity accepted: %v", err)
			}
			if got := openJob(t, j, testJob()).Snapshot(); got != original {
				t.Fatal("rejected request changed the saved identity")
			}
		})
	}
}

func TestVerificationAndValidationCannotBeSkipped(t *testing.T) {
	j := newJournal(t)
	s := openJob(t, j, testJob())
	ctx := context.Background()
	for _, step := range []func(context.Context) error{s.BeginImport, s.BeginValidation, s.Succeed} {
		if err := step(ctx); !errors.Is(err, ErrTransition) {
			t.Fatalf("accepted job skipped verification: %v", err)
		}
	}
	if err := s.StartDownload(ctx); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	for _, metadata := range []artifact.Metadata{
		{},
		{SizeBytes: testJob().SizeBytes + 1, Checksum: testJob().Checksum},
		{SizeBytes: testJob().SizeBytes, Checksum: "sha256:" + strings.Repeat("b", 64)},
	} {
		if err := s.MarkVerified(ctx, metadata); !errors.Is(err, ErrTransition) || s.Snapshot() != before {
			t.Fatalf("untrusted artifact identity was marked verified: %v", err)
		}
	}
	if err := s.MarkVerified(ctx, artifact.Metadata{SizeBytes: testJob().SizeBytes, Checksum: testJob().Checksum}); err != nil {
		t.Fatal(err)
	}
	if err := s.Succeed(ctx); !errors.Is(err, ErrTransition) {
		t.Fatalf("SQL download was reported as successful import: %v", err)
	}
	if err := s.BeginImport(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Succeed(ctx); !errors.Is(err, ErrTransition) {
		t.Fatalf("import exit was reported as validation: %v", err)
	}
}

func TestCancellationAndFailuresAreTerminalWithoutRawErrors(t *testing.T) {
	for _, phase := range []Phase{Accepted, Downloading, Verified, Importing, Validating} {
		t.Run(string(phase), func(t *testing.T) {
			j := newJournal(t)
			s := openJob(t, j, testJob())
			reach(t, s, phase)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			before := s.Snapshot()
			if err := s.Succeed(ctx); !errors.Is(err, context.Canceled) || s.Snapshot() != before {
				t.Fatalf("canceled context changed progress: %v", err)
			}
			if err := s.Fail(Problem("private-password signed-url raw-sql")); !errors.Is(err, ErrTransition) {
				t.Fatalf("raw failure accepted: %v", err)
			}
			if err := s.Cancel(); err != nil {
				t.Fatal(err)
			}
			state := assertSaved(t, s)
			if state.Phase != Canceled || state.WritesStarted != before.WritesStarted || !state.Pending() {
				t.Fatalf("cancellation lost possible destination writes: %+v", state)
			}
			if err := s.Cancel(); err != nil || s.Snapshot() != state {
				t.Fatal("duplicate cancellation changed the terminal result")
			}
			if err := s.Fail(WriteAuthorizationUnavailable); !errors.Is(err, ErrTransition) {
				t.Fatal("failure overwrote cancellation")
			}
			s.Close()
			if got := openJob(t, j, testJob()).Snapshot(); got != state {
				t.Fatal("canceled job changed on restart")
			}
		})
	}
	for _, problem := range []Problem{OriginalKeyUnavailable, WriteAuthorizationUnavailable, UnsafeImport, ArtifactInvalid, DownloadFailed, ResourceLimit, ImportFailed, ValidationFailed} {
		t.Run(string(problem), func(t *testing.T) {
			j := newJournal(t)
			s := openJob(t, j, testJob())
			if problem == ImportFailed || problem == ValidationFailed {
				reach(t, s, Validating)
			}
			if err := s.Fail(problem); err != nil {
				t.Fatal(err)
			}
			state := assertSaved(t, s)
			if state.Phase != Failed || state.Problem != problem || !state.Terminal() {
				t.Fatal("failure lost its classified problem")
			}
			if err := s.Fail(problem); err != nil || s.Snapshot() != state {
				t.Fatal("duplicate failure changed its terminal revision")
			}
		})
	}
}

func TestLateAcknowledgementCannotHideNewProgress(t *testing.T) {
	j := newJournal(t)
	s := openJob(t, j, testJob())
	accepted := s.Snapshot()
	reach(t, s, Succeeded)
	if err := s.Acknowledge(accepted.Revision); err != nil {
		t.Fatal(err)
	}
	terminal := assertSaved(t, s)
	if !terminal.Pending() || terminal.AcknowledgedRevision != accepted.Revision {
		t.Fatal("late acceptance acknowledged the terminal outcome")
	}
	for _, revision := range []uint64{0, terminal.Revision + 1} {
		if err := s.Acknowledge(revision); !errors.Is(err, ErrTransition) || s.Snapshot() != terminal {
			t.Fatalf("invalid acknowledgement changed saved progress: %v", err)
		}
	}
	if err := s.Acknowledge(terminal.Revision); err != nil {
		t.Fatal(err)
	}
	acknowledged := assertSaved(t, s)
	if err := s.Acknowledge(accepted.Revision); err != nil || s.Snapshot() != acknowledged {
		t.Fatal("old acknowledgement moved the saved revision backwards")
	}
}

func TestStatePublicationFailurePoisonsSessionBeforeWrites(t *testing.T) {
	j := newJournal(t)
	s := openJob(t, j, testJob())
	reach(t, s, Verified)
	before := s.Snapshot()
	// A nonempty reserved staging directory simulates a publication failure
	// portably, including when tests run with administrator privileges.
	staged := s.path + ".next"
	if err := os.Mkdir(staged, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "block"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := s.BeginImport(context.Background())
	if !errors.Is(err, ErrState) || strings.Contains(err.Error(), s.path) || s.Snapshot() != before {
		t.Fatalf("failed publication authorized writes or exposed local paths: %v", err)
	}
	for _, step := range []func() error{
		func() error { return s.BeginImport(context.Background()) },
		func() error { return s.Acknowledge(before.Revision) },
		s.Cancel,
	} {
		if err := step(); !errors.Is(err, ErrState) {
			t.Fatalf("session continued after uncertain persistence: %v", err)
		}
	}
	s.Close()
	unlock, err := operation.TryLock(j.operationDir)
	if err != nil {
		t.Fatalf("failed session leaked operation lock: %v", err)
	}
	unlock()
}

func TestInvalidAndUnsafeStateFailsClosed(t *testing.T) {
	for _, failure := range []string{"json", "version", "phase", "raw_problem", "ack", "writes", "unknown", "oversized", "directory"} {
		t.Run(failure, func(t *testing.T) {
			j := newJournal(t)
			s := openJob(t, j, testJob())
			state := s.Snapshot()
			s.Close()
			switch failure {
			case "version":
				state.Version++
			case "phase":
				state.Phase = "unrecognized"
			case "raw_problem":
				state.Phase, state.Problem = Failed, "password-do-not-print"
			case "ack":
				state.AcknowledgedRevision = state.Revision + 1
			case "writes":
				state.Phase = Importing
			}
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "json":
				data = []byte("password-do-not-print")
			case "unknown":
				data = append(data[:len(data)-1], []byte(`,"secret":"password-do-not-print"}`)...)
			case "oversized":
				data = []byte(strings.Repeat("password-do-not-print", 4096))
			}
			if err := os.Remove(s.path); err != nil {
				t.Fatal(err)
			}
			if failure == "directory" {
				err = os.Mkdir(s.path, 0700)
			} else {
				err = securefile.Create(s.path, data)
			}
			if err != nil {
				t.Fatal(err)
			}
			if next, err := j.Open(context.Background(), testJob()); next != nil || !errors.Is(err, ErrState) || strings.Contains(err.Error(), "password-do-not-print") {
				t.Fatalf("unsafe state allowed restore or leaked contents: %v", err)
			}
			unlock, err := operation.TryLock(j.operationDir)
			if err != nil {
				t.Fatalf("failed open leaked lock: %v", err)
			}
			unlock()
		})
	}
}

func TestJobValidationAndFilesystemNames(t *testing.T) {
	for _, id := range []string{"../escape", "a/b", `a\b`, "", ".", strings.Repeat("a", 129)} {
		j := newJournal(t)
		job := testJob()
		job.ID = id
		if s, err := j.Open(context.Background(), job); s != nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe ID accepted: %q, %v", id, err)
		}
	}
	j := newJournal(t)
	paths, destinations := map[string]bool{}, map[string]bool{}
	for _, id := range []string{"CON", "nul", "restore_1", "RESTORE_1"} {
		job := testJob()
		job.ID = id
		s := openJob(t, j, job)
		name := filepath.Base(s.path)
		if paths[name] || destinations[job.Destination()] || len(name) != 69 || len(job.Destination()) > 64 {
			t.Fatal("filesystem names or proposed destinations collide or exceed bounds")
		}
		paths[name], destinations[job.Destination()] = true, true
		s.Close()
	}
	for _, mutate := range []func(*Job){
		func(j *Job) { j.DatabaseID = "../database" },
		func(j *Job) { j.AgentID = "" },
		func(j *Job) { j.BackupID = "../backup" },
		func(j *Job) { j.SizeBytes = 84 },
		func(j *Job) { j.Checksum = "sha256:" + strings.Repeat("A", 64) },
	} {
		job := testJob()
		mutate(&job)
		if s, err := j.Open(context.Background(), job); s != nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid request accepted: %v", err)
		}
	}
}

func TestConcurrentDeliveryAndClosedSessionCannotDriveWork(t *testing.T) {
	j := newJournal(t)
	s := openJob(t, j, testJob())
	var workers sync.WaitGroup
	errorsFound := make(chan error, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			next, err := j.Open(context.Background(), testJob())
			if next != nil {
				next.Close()
			}
			errorsFound <- err
		}()
	}
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if !errors.Is(err, operation.ErrBusy) {
			t.Fatalf("duplicate delivery overlapped a live session: %v", err)
		}
	}
	s.Close()
	s.Close()
	before := s.Snapshot()
	for _, step := range []func() error{
		func() error { return s.StartDownload(context.Background()) },
		func() error { return s.Acknowledge(before.Revision) },
		s.Cancel,
	} {
		if err := step(); !errors.Is(err, ErrClosed) || s.Snapshot() != before {
			t.Fatalf("closed session changed state: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if next, err := j.Open(ctx, testJob()); next != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled delivery opened a session: %v", err)
	}
	openJob(t, j, testJob())
}
