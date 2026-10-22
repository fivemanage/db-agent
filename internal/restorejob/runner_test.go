package restorejob

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/operation"
	"github.com/fivemanage/db-agent/internal/restore"
	"github.com/fivemanage/db-agent/internal/restoreplan"
	"github.com/fivemanage/db-agent/internal/securefile"
	"gopkg.in/yaml.v3"
)

const runnerSQL = "CREATE DATABASE source_game CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; USE source_game; CREATE TABLE players (id INT PRIMARY KEY, note TEXT) ENGINE=InnoDB; INSERT INTO players VALUES (1,'private-sql-data');"

type runnerLookup struct {
	target controlplane.DownloadBackupResponse
	calls  atomic.Int32
}

func (l *runnerLookup) DownloadBackup(context.Context, string) (controlplane.DownloadBackupResponse, error) {
	l.calls.Add(1)
	return l.target, nil
}

type runnerFixture struct {
	runner   *Runner
	job      Job
	settings config.Config
	lookup   *runnerLookup
	gets     atomic.Int32
}

func setupRunner(t *testing.T, plain string) *runnerFixture {
	t.Helper()
	f := &runnerFixture{}
	root := t.TempDir()
	k, err := key.Parse("dbak1_" + strings.Repeat("A", 42) + "E")
	if err != nil {
		t.Fatal(err)
	}
	f.settings.Database = config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "backup-private", Databases: []string{"source_game"}}
	f.settings.Restore = config.RestoreConfig{Authorized: true, Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "restore_setup", Password: "authority-private"}
	f.settings.Encryption = config.EncryptionConfig{KeyFile: "encryption.key", KeyID: k.ID(), KeyAcknowledged: true}
	if err := securefile.Create(filepath.Join(root, "encryption.key"), []byte(k.RecoveryString()+"\n")); err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	meta, err := artifact.Encrypt(context.Background(), &encrypted, strings.NewReader(plain), k, artifact.Zstd)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gets.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("local/API credentials entered storage request")
		}
		w.Write(encrypted.Bytes())
	}))
	t.Cleanup(server.Close)
	f.job = testJob()
	f.job.SizeBytes, f.job.Checksum = meta.SizeBytes, meta.Checksum
	f.lookup = &runnerLookup{target: controlplane.DownloadBackupResponse{DownloadURL: server.URL + "/backup?signature=private-url", ExpiresAt: time.Now().Add(time.Hour), SizeBytes: meta.SizeBytes, Checksum: meta.Checksum}}
	path := filepath.Join(root, "config.yaml")
	f.runner, err = NewRunner(path, f.job.Binding, f.lookup, Options{Download: restore.Options{Transport: server.Client().Transport, ReserveBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	f.save(t)
	return f
}

func (f *runnerFixture) save(t *testing.T) {
	t.Helper()
	data, err := yaml.Marshal(f.settings)
	if err != nil {
		t.Fatal(err)
	}
	var writeErr error
	if _, err := os.Lstat(f.runner.path); errors.Is(err, os.ErrNotExist) {
		writeErr = securefile.Create(f.runner.path, data)
	} else {
		writeErr = os.WriteFile(f.runner.path, data, 0600)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
}

func assertNoWorkspace(t *testing.T, runner *Runner) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(runner.journal.dir, ".restore-work-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal("handled restore left a private plaintext workspace")
	}
}

func TestRunnerJoinsRecoveryPlanJournalAndTerminalReplay(t *testing.T) {
	f := setupRunner(t, runnerSQL)
	var phases []Phase
	f.runner.options.Observe = func(state State) { phases = append(phases, state.Phase) }
	imports := 0
	f.runner.importPlan = func(ctx context.Context, backup config.DatabaseConfig, access config.RestoreConfig, plan *restoreplan.Plan, timeout time.Duration, start, validate func(context.Context) error) error {
		imports++
		assertNoWorkspace(t, f.runner)
		if plan.Destination() != f.job.Destination() || len(plan.Tables()) != 1 || plan.Tables()[0].Rows != 1 || backup.User == access.User {
			t.Fatal("runner lost scope/data validation or reused the backup account")
		}
		if err := start(ctx); err != nil {
			return err
		}
		state, err := f.runner.Snapshot(f.job)
		if err != nil || state.Phase != Importing || !state.WritesStarted {
			t.Fatal("write intent was not durably visible before import")
		}
		if unlock, err := operation.TryLock(f.runner.journal.operationDir); !errors.Is(err, operation.ErrBusy) {
			if unlock != nil {
				unlock()
			}
			t.Fatal("runner released the shared backup/restore lock during SQL")
		}
		return validate(ctx)
	}
	state, err := f.runner.Run(context.Background(), f.job)
	if err != nil || state.Phase != Succeeded || !state.Pending() || !state.WritesStarted || imports != 1 || f.gets.Load() != 1 {
		t.Fatalf("runner did not finish the complete local journey: %+v %v", state, err)
	}
	if want := []Phase{Accepted, Downloading, Verified, Importing, Validating, Succeeded}; !reflect.DeepEqual(phases, want) {
		t.Fatalf("wrong durable progress sequence: %v", phases)
	}
	for _, secret := range []string{"private-sql-data", "authority-private", "backup-private", "private-url", "dbak1_"} {
		digest := filepath.Join(f.runner.journal.dir, "*.json")
		paths, _ := filepath.Glob(digest)
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil || bytes.Contains(data, []byte(secret)) {
				t.Fatal("durable restore state leaked a key, credential, SQL, or signed URL")
			}
		}
	}
	assertNoWorkspace(t, f.runner)
	if err := os.Remove(f.runner.path); err != nil {
		t.Fatal(err)
	}
	replay, err := f.runner.Run(context.Background(), f.job)
	if err != nil || replay != state || imports != 1 || f.gets.Load() != 1 || f.lookup.calls.Load() != 1 {
		t.Fatal("terminal redelivery accessed config/network or repeated an import")
	}
}

func TestRunnerPreImportFailuresNeverAuthorizeWrites(t *testing.T) {
	for _, scenario := range []string{"missing_key", "wrong_key", "missing_authorization", "multiple_schemas", "changed_metadata", "unsupported_sql", "output_limit", "disk_reserve", "revoked_before_write"} {
		t.Run(scenario, func(t *testing.T) {
			plain := runnerSQL
			if scenario == "unsupported_sql" {
				plain += "CREATE PROCEDURE unsafe() SELECT 1;"
			}
			f := setupRunner(t, plain)
			want := WriteAuthorizationUnavailable
			switch scenario {
			case "missing_key":
				os.Remove(filepath.Join(filepath.Dir(f.runner.path), "encryption.key"))
				want = OriginalKeyUnavailable
			case "wrong_key":
				wrong, err := key.Parse("dbak1_" + strings.Repeat("A", 42) + "I")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(f.runner.path), "encryption.key"), []byte(wrong.RecoveryString()+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f.settings.Encryption.KeyID = wrong.ID()
				f.save(t)
				want = ArtifactInvalid
			case "missing_authorization":
				f.settings.Restore.Authorized = false
				f.save(t)
			case "multiple_schemas":
				f.settings.Database.Databases = append(f.settings.Database.Databases, "other")
				f.save(t)
				want = UnsafeImport
			case "changed_metadata":
				f.lookup.target.SizeBytes++
				want = ArtifactInvalid
			case "unsupported_sql":
				want = UnsafeImport
			case "output_limit":
				f.runner.options.Download.MaxOutputBytes = 10
				want = ResourceLimit
			case "disk_reserve":
				f.runner.options.Download.ReserveBytes = 1 << 62
				want = ResourceLimit
			}
			f.runner.importPlan = func(ctx context.Context, _ config.DatabaseConfig, _ config.RestoreConfig, _ *restoreplan.Plan, _ time.Duration, start, validate func(context.Context) error) error {
				if scenario != "revoked_before_write" {
					t.Fatal("pre-import failure reached database execution")
				}
				f.settings.Restore.Authorized = false
				f.save(t)
				if err := start(ctx); err != nil {
					return err
				}
				t.Fatal("local revocation was ignored")
				return nil
			}
			state, err := f.runner.Run(context.Background(), f.job)
			if err == nil || state.Phase != Failed || state.Problem != want || state.WritesStarted {
				t.Fatalf("unsafe pre-import failure: %+v %v", state, err)
			}
			assertNoWorkspace(t, f.runner)
			before := f.lookup.calls.Load()
			replay, err := f.runner.Run(context.Background(), f.job)
			if err != nil || replay != state || f.lookup.calls.Load() != before {
				t.Fatal("terminal failed job was downloaded/imported again")
			}
		})
	}
}

func TestRunnerNeverWritesPastFailedDurableIntent(t *testing.T) {
	f := setupRunner(t, runnerSQL)
	enteredWrite := false
	f.runner.importPlan = func(ctx context.Context, _ config.DatabaseConfig, _ config.RestoreConfig, _ *restoreplan.Plan, _ time.Duration, start, validate func(context.Context) error) error {
		paths, err := filepath.Glob(filepath.Join(f.runner.journal.dir, "*.json"))
		if err != nil || len(paths) != 1 {
			t.Fatal("missing durable restore record")
		}
		staged := paths[0] + ".next"
		if err := os.Mkdir(staged, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staged, "block"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := start(ctx); err != nil {
			return err
		}
		enteredWrite = true
		return nil
	}
	state, err := f.runner.Run(context.Background(), f.job)
	if !errors.Is(err, ErrState) || enteredWrite || state.WritesStarted || state.Phase != Verified {
		t.Fatal("runner authorized database writes without durable intent")
	}
}

func TestRunnerCancellationValidationAndUncertainCleanup(t *testing.T) {
	for _, scenario := range []string{"cancel", "uncertain", "validation", "raw_error"} {
		t.Run(scenario, func(t *testing.T) {
			f := setupRunner(t, runnerSQL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.runner.importPlan = func(ctx context.Context, _ config.DatabaseConfig, _ config.RestoreConfig, _ *restoreplan.Plan, _ time.Duration, start, validate func(context.Context) error) error {
				if err := start(ctx); err != nil {
					return err
				}
				switch scenario {
				case "cancel":
					cancel()
					return ctx.Err()
				case "uncertain":
					cancel()
					return errors.Join(ctx.Err(), database.ErrRestoreUncertain)
				case "validation":
					if err := validate(ctx); err != nil {
						return err
					}
					return database.ErrRestoreValidation
				default:
					return errors.New("private-sql-data authority-private private-url")
				}
			}
			state, err := f.runner.Run(ctx, f.job)
			if err == nil || !state.WritesStarted || strings.Contains(err.Error(), "authority-private") || strings.Contains(err.Error(), "private-sql-data") {
				t.Fatal("runner lost write intent or leaked private failure text")
			}
			switch scenario {
			case "cancel":
				if state.Phase != Canceled || !errors.Is(err, context.Canceled) {
					t.Fatal("joined cancellation did not persist a canceled outcome")
				}
			case "uncertain":
				if state.Phase != Failed || state.Problem != ImportInterrupted {
					t.Fatal("uncertain server cleanup was falsely reported as clean cancellation")
				}
			case "validation":
				if state.Phase != Failed || state.Problem != ValidationFailed {
					t.Fatal("failed validation became success")
				}
			default:
				if state.Phase != Failed || state.Problem != ImportFailed {
					t.Fatal("failed import became success")
				}
			}
			assertNoWorkspace(t, f.runner)
		})
	}
}
