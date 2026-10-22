package restorejob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/operation"
	"github.com/fivemanage/db-agent/internal/securefile"
)

type Journal struct {
	dir          string
	operationDir string
	binding      Binding
}

var journalNamePattern = regexp.MustCompile(`^[a-f0-9]{64}\.json$`)

func (j *Journal) pendingTerminal() ([]State, error) {
	entries, err := os.ReadDir(j.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &stateError{err}
	}
	var states []State
	for _, entry := range entries {
		if !journalNamePattern.MatchString(entry.Name()) {
			continue
		}
		state, err := load(filepath.Join(j.dir, entry.Name()))
		if err != nil {
			return nil, &stateError{err}
		}
		if state.Job.Binding == j.binding && state.Terminal() && state.Pending() {
			states = append(states, state)
			if len(states) == 10 {
				break
			}
		}
	}
	return states, nil
}

// Read observes durable progress without obtaining the operation lock or
// treating a live import as abandoned. Open alone performs crash recovery.
func (j *Journal) Read(job Job) (State, error) {
	if !job.valid() {
		return State{}, ErrInvalid
	}
	if job.Binding != j.binding {
		return State{}, ErrBinding
	}
	digest := sha256.Sum256([]byte(job.ID))
	path := filepath.Join(j.dir, hex.EncodeToString(digest[:])+".json")
	var state State
	var err error
	for range 3 {
		state, err = load(path)
		if !errors.Is(err, securefile.ErrUnsafe) {
			break
		}
	}
	if err != nil {
		return State{}, &stateError{err}
	}
	if state.Job != job {
		return State{}, ErrConflict
	}
	return state, nil
}

func New(dir, operationDir string, binding Binding) (*Journal, error) {
	if dir == "" || operationDir == "" || !binding.valid() {
		return nil, ErrInvalid
	}
	return &Journal{dir: dir, operationDir: operationDir, binding: binding}, nil
}

// Open holds the shared backup/restore lock until Close. Only one session may
// drive work; terminal sessions replay saved results without authorizing writes.
func (j *Journal) Open(ctx context.Context, job Job) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !job.valid() {
		return nil, ErrInvalid
	}
	if job.Binding != j.binding {
		return nil, ErrBinding
	}
	unlock, err := operation.TryLock(j.operationDir)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Hash names to avoid path injection, Windows reserved names, and collisions
	// between case-sensitive IDs on case-insensitive filesystems.
	digest := sha256.Sum256([]byte(job.ID))
	path := filepath.Join(j.dir, hex.EncodeToString(digest[:])+".json")
	state, err := load(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, &stateError{err}
	}
	session := &Session{path: path, unlock: unlock, state: state}
	switch {
	case errors.Is(err, os.ErrNotExist):
		err = session.save(State{Version: 1, Job: job, Phase: Accepted, Revision: 1})
	case state.Job != job:
		return nil, ErrConflict
	case state.Terminal() || state.Phase == Accepted:
		// An earlier process may have observed a directory-sync error after a
		// successful rename. Reestablish durability before returning acceptance
		// or a terminal report, without creating another progress revision.
		err = session.save(state)
	case state.WritesStarted:
		err = session.finish(Failed, ImportInterrupted)
	default:
		// SQL left by an earlier process is never trusted just because it exists.
		// Pre-import recovery must download/authenticate/preflight again.
		err = session.advance(Accepted)
	}
	if err != nil {
		return nil, err
	}
	opened = true
	return session, nil
}

// Session methods are serialized, but callers must also stop and join download
// and import workers before Cancel, Fail, or Close. Close never cancels SQL.
type Session struct {
	mu     sync.Mutex
	path   string
	unlock func()
	state  State
	broken bool
	closed bool
}

func (s *Session) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.unlock()
	}
}

func (s *Session) StartDownload(ctx context.Context) error {
	return s.transition(ctx, Accepted, Downloading)
}

// MarkVerified accepts metadata only after the engine has fully authenticated,
// decompressed, checked EOF, and synced the private SQL output. It is not itself
// an artifact verifier or an SQL safety check.
func (s *Session) MarkVerified(ctx context.Context, metadata artifact.Metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return err
	}
	if s.state.Phase != Downloading || metadata.SizeBytes != s.state.Job.SizeBytes || metadata.Checksum != s.state.Job.Checksum {
		return ErrTransition
	}
	return s.advance(Verified)
}

// BeginImport must succeed durably before the first destination write. The
// caller must first validate local write authorization and the isolated plan.
func (s *Session) BeginImport(ctx context.Context) error {
	return s.transition(ctx, Verified, Importing)
}

func (s *Session) BeginValidation(ctx context.Context) error {
	return s.transition(ctx, Importing, Validating)
}

// Succeed is allowed only after independent post-import validation. Import
// process exit alone is not a verified usable destination.
func (s *Session) Succeed(ctx context.Context) error {
	return s.transition(ctx, Validating, Succeeded)
}

func (s *Session) transition(ctx context.Context, from, to Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return err
	}
	if s.state.Phase == to && s.state.Terminal() {
		return nil
	}
	if s.state.Phase != from {
		return ErrTransition
	}
	return s.advance(to)
}

// Terminal outcomes must still be saved when the work context is canceled.
func (s *Session) Fail(problem Problem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(context.Background()); err != nil {
		return err
	}
	if !problem.valid() {
		return ErrTransition
	}
	return s.finish(Failed, problem)
}

func (s *Session) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(context.Background()); err != nil {
		return err
	}
	return s.finish(Canceled, NoProblem)
}

// Acknowledge only the exact revision accepted by the server. A late response
// for older progress cannot mark a newer outcome as delivered.
func (s *Session) Acknowledge(revision uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(context.Background()); err != nil {
		return err
	}
	if revision == 0 || revision > s.state.Revision {
		return ErrTransition
	}
	if revision <= s.state.AcknowledgedRevision {
		return nil
	}
	next := s.state
	next.AcknowledgedRevision = revision
	return s.save(next)
}

func (s *Session) ready(ctx context.Context) error {
	if s.closed {
		return ErrClosed
	}
	if s.broken {
		return ErrState
	}
	return ctx.Err()
}

func (s *Session) finish(phase Phase, problem Problem) error {
	if s.state.Terminal() {
		if s.state.Phase == phase && s.state.Problem == problem {
			return nil
		}
		return ErrTransition
	}
	next := s.state
	next.Phase, next.Problem = phase, problem
	return s.commit(next)
}

func (s *Session) advance(phase Phase) error {
	next := s.state
	next.Phase = phase
	if phase == Importing {
		next.WritesStarted = true
	}
	return s.commit(next)
}

func (s *Session) commit(next State) error {
	if next.Revision == ^uint64(0) {
		return ErrState
	}
	next.Revision++
	if !next.valid() {
		return ErrTransition
	}
	return s.save(next)
}

func load(path string) (State, error) {
	data, err := securefile.Read(path, 4096)
	if err != nil {
		return State{}, err
	}
	var state State
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&state) != nil || !state.valid() || decoder.Decode(new(any)) != io.EOF {
		return State{}, ErrState
	}
	return state, nil
}

func (s *Session) save(next State) error {
	if err := write(s.path, next); err != nil {
		// Replacement may have succeeded before directory sync failed. Never
		// continue with the old in-memory state or authorize a write after error.
		s.broken = true
		return &stateError{err}
	}
	s.state = next
	return nil
}

func write(path string, state State) error {
	if !state.valid() {
		return ErrState
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > 4096 {
		return ErrState
	}
	staged := path + ".next"
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := securefile.Create(staged, data); err != nil {
		return err
	}
	defer os.Remove(staged)
	return securefile.ReplaceDurable(staged, path)
}

type stateError struct{ cause error }

func (e *stateError) Error() string        { return ErrState.Error() }
func (e *stateError) Unwrap() error        { return e.cause }
func (e *stateError) Is(target error) bool { return target == ErrState }
