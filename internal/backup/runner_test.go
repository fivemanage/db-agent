package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/transfer"
)

type testPlane struct {
	mu            sync.Mutex
	url           string
	initiateError error
	completeError error
	initiations   []controlplane.InitiateBackupRequest
	completions   []controlplane.CompleteBackupRequest
	onComplete    func()
}

func (p *testPlane) InitiateBackup(_ context.Context, in controlplane.InitiateBackupRequest) (controlplane.InitiateBackupResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initiations = append(p.initiations, in)
	return controlplane.InitiateBackupResponse{BackupID: "backup_1", UploadURL: p.url, ExpiresAt: time.Now().Add(time.Hour)}, p.initiateError
}

func (p *testPlane) CompleteBackup(ctx context.Context, _ string, in controlplane.CompleteBackupRequest) error {
	p.mu.Lock()
	p.completions = append(p.completions, in)
	err, onComplete := p.completeError, p.onComplete
	p.mu.Unlock()
	if onComplete != nil && ctx.Err() == nil {
		onComplete()
	}
	return err
}

func backupKey(t *testing.T) key.Key {
	t.Helper()
	k, err := key.Parse("dbak1_" + strings.Repeat("A", 42) + "E")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testRunner(t *testing.T, dir string, plane transfer.ControlPlane, storage *httptest.Server, options transfer.Options) *Runner {
	t.Helper()
	// New clones the default transport. Trust only this isolated test server in
	// that clone, without changing the production uploader's TLS policy.
	previous := http.DefaultTransport
	if storage != nil {
		http.DefaultTransport = storage.Client().Transport
	}
	uploader, err := transfer.New(dir, plane, options)
	http.DefaultTransport = previous
	if err != nil {
		t.Fatal(err)
	}
	runner, err := New(dir, uploader)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func plan(t *testing.T, compression artifact.Compression, dump func(context.Context, io.Writer) error) func(context.Context) (Plan, error) {
	t.Helper()
	k := backupKey(t)
	return func(context.Context) (Plan, error) {
		return Plan{Dump: dump, Key: k, Compression: compression}, nil
	}
}

func await(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stage did not finish")
	}
}

func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("backup workers did not join")
		return nil
	}
}

func assertFiles(t *testing.T, dir string, pending, ciphertext bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{".lock": true, ".run.lock": true, "run.json": true}
	if pending {
		allowed["pending.json"] = true
	}
	if ciphertext {
		allowed["ciphertext.bak"] = true
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			t.Fatalf("unexpected leftover artifact %s", entry.Name())
		}
	}
	for name, wanted := range map[string]bool{"pending.json": pending, "ciphertext.bak": ciphertext} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != wanted {
			t.Fatalf("%s existence: %v", name, err)
		}
	}
}

func TestPipelineLifecycleAndSharedHeartbeatState(t *testing.T) {
	for _, compression := range []artifact.Compression{artifact.Zstd, artifact.Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			dumpStarted, allowDump := make(chan struct{}), make(chan struct{})
			uploadStarted, allowUpload := make(chan struct{}), make(chan struct{})
			completionStarted, allowCompletion := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			plain := "CREATE TABLE game_private (password TEXT); INSERT INTO game_private VALUES ('private-secret');"
			var ciphertext []byte
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Method != http.MethodPut {
					t.Error("unsafe object storage request")
				}
				ciphertext, _ = io.ReadAll(r.Body)
				close(uploadStarted)
				select {
				case <-allowUpload:
				case <-ctx.Done():
				}
				w.WriteHeader(204)
			}))
			defer storage.Close()
			defer cancel()
			p := &testPlane{url: storage.URL, onComplete: func() {
				close(completionStarted)
				select {
				case <-allowCompletion:
				case <-ctx.Done():
				}
			}}
			dir := t.TempDir()
			r := testRunner(t, dir, p, storage, transfer.Options{})
			other := testRunner(t, dir, p, storage, transfer.Options{})
			done := make(chan error, 1)
			go func() {
				done <- r.Run(ctx, plan(t, compression, func(ctx context.Context, dst io.Writer) error {
					close(dumpStarted)
					select {
					case <-allowDump:
					case <-ctx.Done():
						return ctx.Err()
					}
					_, err := io.WriteString(dst, plain)
					return err
				}))
			}()

			await(t, dumpStarted)
			if state := other.HeartbeatState(controlplane.State{Status: controlplane.Idle}); state.Status != controlplane.BackingUp {
				t.Fatalf("wrong dump heartbeat: %+v", state)
			}
			prepared := false
			if err := other.Run(ctx, func(context.Context) (Plan, error) { prepared = true; return Plan{}, nil }); !errors.Is(err, ErrRunning) || prepared {
				t.Fatalf("second trigger entered the run: %v", err)
			}
			if err := other.Resume(ctx); !errors.Is(err, ErrRunning) {
				t.Fatalf("recovery overlapped the dump: %v", err)
			}

			close(allowDump)
			await(t, uploadStarted)
			if state := other.HeartbeatState(controlplane.State{Status: controlplane.Idle}); state.Status != controlplane.Uploading {
				t.Fatalf("wrong upload heartbeat: %+v", state)
			}
			close(allowUpload)
			await(t, completionStarted)
			snapshot, err := other.Snapshot()
			if err != nil || snapshot.Activity != controlplane.Uploading || snapshot.Latest.Status != "success" || !snapshot.Latest.Pending {
				t.Fatalf("completion not represented: %+v %v", snapshot, err)
			}
			close(allowCompletion)
			if err := awaitResult(t, done); err != nil {
				t.Fatal(err)
			}

			snapshot, err = other.Snapshot()
			digest := sha256.Sum256(ciphertext)
			if err != nil || snapshot.Activity != controlplane.Idle || snapshot.Latest.Pending || snapshot.LastError != controlplane.NoProblem ||
				snapshot.Latest.SizeBytes != int64(len(ciphertext)) || snapshot.Latest.Checksum != "sha256:"+hex.EncodeToString(digest[:]) || snapshot.Latest.DurationSeconds <= 0 {
				t.Fatalf("wrong durable completion: %+v %v", snapshot, err)
			}
			var restored bytes.Buffer
			if _, err := artifact.Decrypt(ctx, &restored, bytes.NewReader(ciphertext), backupKey(t)); err != nil || restored.String() != plain {
				t.Fatalf("pipeline artifact cannot be restored: %v", err)
			}
			data, _ := os.ReadFile(r.statePath())
			if bytes.Contains(data, []byte("private-secret")) || bytes.Contains(data, []byte(backupKey(t).RecoveryString())) || bytes.Contains(ciphertext, []byte(plain)) {
				t.Fatal("local metadata or storage contains plaintext/key material")
			}
			assertFiles(t, dir, false, false)
		})
	}
}

func TestPipelineStageFailuresCleanAndJoin(t *testing.T) {
	for _, stage := range []string{"dump", "compression", "encryption", "spool", "upload"} {
		t.Run(stage, func(t *testing.T) {
			var puts atomic.Int32
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				puts.Add(1)
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(400)
			}))
			defer storage.Close()
			p := &testPlane{url: storage.URL}
			dir := t.TempDir()
			options := transfer.Options{}
			if stage == "spool" {
				options.MaxBytes = 50
			}
			r := testRunner(t, dir, p, storage, options)
			joined := make(chan struct{})
			var dumpCalls atomic.Int32
			prepare := plan(t, artifact.Zstd, func(_ context.Context, dst io.Writer) error {
				dumpCalls.Add(1)
				defer close(joined)
				if _, err := io.WriteString(dst, "sensitive SQL and database-password"); err != nil {
					return err
				}
				if stage == "dump" {
					return errors.New("database-password sensitive raw process output")
				}
				return nil
			})
			if stage == "compression" || stage == "encryption" {
				original := prepare
				prepare = func(ctx context.Context) (Plan, error) {
					p, err := original(ctx)
					if stage == "compression" {
						p.Compression = "unsupported"
					} else {
						p.Key = key.Key{}
					}
					return p, err
				}
			}

			err := r.Run(context.Background(), prepare)
			if err == nil || strings.Contains(err.Error(), "database-password") || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("unsafe/missing failure: %v", err)
			}
			if dumpCalls.Load() > 0 {
				await(t, joined)
			}
			if (stage == "compression" || stage == "encryption") && dumpCalls.Load() != 0 {
				t.Fatal("dump started before transform validation")
			}
			if len(p.completions) != 1 || p.completions[0].Status != "failed" {
				t.Fatal("failure was not reported")
			}
			problem := controlplane.BackupFailed
			if stage == "upload" {
				problem = controlplane.UploadFailed
			} else if puts.Load() != 0 {
				t.Fatal("partial backup was uploaded")
			}
			snapshot, stateErr := r.Snapshot()
			if stateErr != nil || snapshot.Activity != controlplane.Error || snapshot.LastError != problem || snapshot.Latest.Status != "failed" || snapshot.Latest.Pending {
				t.Fatalf("wrong terminal state: %+v %v", snapshot, stateErr)
			}
			assertFiles(t, dir, false, false)
			if err := r.Resume(context.Background()); err != nil {
				t.Fatalf("failure leaked run lock: %v", err)
			}
		})
	}
}

func TestRecoveryPreservesIdentityAndSuccessfulPendingCompletion(t *testing.T) {
	for _, failedStage := range []string{"initiate", "complete"} {
		t.Run(failedStage, func(t *testing.T) {
			var puts, dumps atomic.Int32
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				puts.Add(1)
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(204)
			}))
			defer storage.Close()
			p := &testPlane{url: storage.URL}
			if failedStage == "initiate" {
				p.initiateError = &controlplane.ResponseError{StatusCode: 503}
			} else {
				p.completeError = &controlplane.ResponseError{StatusCode: 503}
			}
			dir := t.TempDir()
			r := testRunner(t, dir, p, storage, transfer.Options{})
			prepare := plan(t, artifact.Gzip, func(_ context.Context, dst io.Writer) error {
				dumps.Add(1)
				_, err := io.WriteString(dst, "offline database dump")
				return err
			})
			if err := r.Run(context.Background(), prepare); !errors.Is(err, ErrQueued) {
				t.Fatalf("outage not queued: %v", err)
			}
			snapshot, err := r.Snapshot()
			if err != nil || snapshot.Activity != controlplane.Error || snapshot.LastError != controlplane.UploadFailed || !snapshot.Latest.Pending {
				t.Fatalf("wrong queued state: %+v %v", snapshot, err)
			}
			assertFiles(t, dir, true, failedStage == "initiate")
			if err := r.Run(context.Background(), prepare); !errors.Is(err, ErrQueued) || dumps.Load() != 1 {
				t.Fatalf("queued run was overwritten or dumped again: %v", err)
			}

			p.initiateError, p.completeError = nil, nil
			restarted := testRunner(t, dir, p, storage, transfer.Options{})
			if err := restarted.Resume(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, err = restarted.Snapshot()
			if err != nil || snapshot.Activity != controlplane.Idle || snapshot.Latest.Status != "success" || snapshot.Latest.Pending || puts.Load() != 1 || dumps.Load() != 1 {
				t.Fatalf("bad recovery: %+v %v", snapshot, err)
			}
			for _, request := range p.initiations {
				if !reflect.DeepEqual(request, p.initiations[0]) {
					t.Fatal("recovery changed request identity")
				}
			}
			for _, completion := range p.completions {
				if !reflect.DeepEqual(completion, p.completions[0]) {
					t.Fatal("recovery changed terminal metadata")
				}
			}
			assertFiles(t, dir, false, false)
		})
	}
}

func TestCancellationUnblocksProducerAndUploadThrottle(t *testing.T) {
	for _, stage := range []string{"dump", "upload"} {
		t.Run(stage, func(t *testing.T) {
			started, joined := make(chan struct{}), make(chan struct{})
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stage == "upload" {
					close(started)
				}
				io.Copy(io.Discard, r.Body)
			}))
			defer storage.Close()
			p := &testPlane{url: storage.URL}
			dir := t.TempDir()
			options := transfer.Options{}
			if stage == "upload" {
				options.BytesPerSecond = 1
			}
			r := testRunner(t, dir, p, storage, options)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			data := make([]byte, 128<<10)
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
			prepare := plan(t, artifact.Zstd, func(ctx context.Context, dst io.Writer) error {
				defer close(joined)
				if stage == "upload" {
					_, err := dst.Write(data)
					return err
				}
				close(started)
				for {
					if _, err := dst.Write(data); err != nil {
						return err
					}
					if err := ctx.Err(); err != nil {
						return err
					}
				}
			})
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx, prepare) }()
			await(t, started)
			cancel()
			if err := awaitResult(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("wrong cancellation: %v", err)
			}
			await(t, joined)
			assertFiles(t, dir, true, false)
			if err := r.Resume(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(p.completions) != 1 || p.completions[0].Status != "failed" {
				t.Fatal("cancellation not reported as failed")
			}
			expectedProblem := controlplane.BackupFailed
			if stage == "upload" {
				expectedProblem = controlplane.UploadFailed
			}
			state := r.HeartbeatState(controlplane.State{Status: controlplane.Idle})
			if state.Status != controlplane.Error || state.Problem != expectedProblem {
				t.Fatalf("acknowledging a failed run lost its terminal state: %+v", state)
			}
			assertFiles(t, dir, false, false)
		})
	}
}

func TestDiskHeadroomRejectsBeforeDumpOrSpool(t *testing.T) {
	dir := t.TempDir()
	p := &testPlane{}
	r := testRunner(t, dir, p, nil, transfer.Options{FreeSpaceReserve: 1 << 62})
	var dumped bool
	err := r.Run(context.Background(), plan(t, artifact.Zstd, func(context.Context, io.Writer) error { dumped = true; return nil }))
	if !errors.Is(err, ErrSpace) || dumped || len(p.initiations) != 0 {
		t.Fatalf("dump/network started without disk headroom: %v", err)
	}
	assertFiles(t, dir, false, false)
}

func TestStageFailureStillRemovesCiphertextWhenSavingFailureFails(t *testing.T) {
	dir := t.TempDir()
	r := testRunner(t, dir, &testPlane{}, nil, transfer.Options{})
	joined := make(chan struct{})
	err := r.Run(context.Background(), plan(t, artifact.Gzip, func(_ context.Context, dst io.Writer) error {
		defer close(joined)
		if _, err := io.WriteString(dst, strings.Repeat("private SQL", 1000)); err != nil {
			return err
		}
		// Refuse removal of the reserved state staging path, simulating a state
		// publication failure after partial ciphertext has already been written.
		staged := filepath.Join(dir, "pending.json.next")
		if err := os.Mkdir(staged, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(staged, "block"), nil, 0600); err != nil {
			return err
		}
		return errors.New("dump failed with private raw output")
	}))
	if !errors.Is(err, ErrState) {
		t.Fatalf("state failure not surfaced: %v", err)
	}
	await(t, joined)
	if _, err := os.Stat(filepath.Join(dir, "ciphertext.bak")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("state-save failure retained partial ciphertext")
	}
}
