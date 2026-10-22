//go:build integration

package restoreproof

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/restore"
	"github.com/fivemanage/db-agent/internal/restorejob"
	"github.com/fivemanage/db-agent/internal/securefile"
	"gopkg.in/yaml.v3"
)

type executionFixture struct {
	f        *fixture
	path     string
	job      restorejob.Job
	settings config.Config
	storage  *httptest.Server
	api      *httptest.Server
	gets     atomic.Int32
	lookups  atomic.Int32
}

func newExecutionFixture(f *fixture, sql []byte) *executionFixture {
	f.t.Helper()
	x := &executionFixture{f: f}
	backup, access := authorityFixture(f, "")
	material := sha256.Sum256([]byte("public full-executor fixture key"))
	k, err := key.Parse(key.Prefix + base64.RawURLEncoding.EncodeToString(material[:]))
	if err != nil {
		f.t.Fatal("cannot create fixture key")
	}
	var encrypted bytes.Buffer
	meta, err := artifact.Encrypt(f.ctx, &encrypted, bytes.NewReader(sql), k, artifact.Zstd)
	if err != nil {
		f.t.Fatal("cannot encrypt native executor fixture")
	}
	x.job = restorejob.Job{ID: f.user + "_job", Binding: restorejob.Binding{DatabaseID: "fixture_database", AgentID: "fixture_agent"}, BackupID: "fixture_backup", SizeBytes: meta.SizeBytes, Checksum: meta.Checksum}
	f.target = x.job.Destination()
	x.storage = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.gets.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			f.t.Error("executor forwarded local or agent credentials to storage")
		}
		_, _ = w.Write(encrypted.Bytes())
	}))
	f.t.Cleanup(x.storage.Close)
	credential := "dac_" + strings.Repeat("A", 43)
	x.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.lookups.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+credential || r.URL.Path != "/api/v1/backups/fixture_backup/download" {
			f.t.Error("executor did not use the authenticated backup lookup")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(controlplane.DownloadBackupResponse{DownloadURL: x.storage.URL + "/artifact?signature=public_fixture", ExpiresAt: time.Now().Add(time.Hour), SizeBytes: meta.SizeBytes, Checksum: meta.Checksum})
	}))
	f.t.Cleanup(x.api.Close)
	dir := f.t.TempDir()
	x.path = filepath.Join(dir, "config.yaml")
	x.settings = config.Config{AgentCredential: credential, APIURL: x.api.URL, Database: backup, Restore: access,
		Encryption: config.EncryptionConfig{KeyFile: "encryption.key", KeyID: k.ID(), KeyAcknowledged: true}}
	if err := securefile.Create(filepath.Join(dir, "encryption.key"), []byte(k.RecoveryString()+"\n")); err != nil {
		f.t.Fatal("cannot save fixture key")
	}
	data, err := yaml.Marshal(x.settings)
	if err != nil || securefile.Create(x.path, data) != nil {
		f.t.Fatal("cannot save private executor fixture config")
	}
	return x
}

func (x *executionFixture) runner(options restorejob.Options) *restorejob.Runner {
	x.f.t.Helper()
	client, err := controlplane.New(x.settings.APIURL, x.settings.AgentCredential, "0.1.0", nil)
	if err != nil {
		x.f.t.Fatal(err)
	}
	options.Download.Transport = x.storage.Client().Transport
	options.Download.ReserveBytes = 1 << 20
	runner, err := restorejob.NewRunner(x.path, x.job.Binding, client, options)
	if err != nil {
		x.f.t.Fatal(err)
	}
	return runner
}

func childUser(job restorejob.Job) string {
	digest := sha256.Sum256([]byte(job.Destination()))
	return "fmr_" + hex.EncodeToString(digest[:14])
}

func registerExecutionCleanup(x *executionFixture) {
	x.f.t.Cleanup(func() {
		// Every target is a random job-owned name in a disposable container.
		_, _ = x.f.admin.Exec("DROP DATABASE IF EXISTS " + qi(x.job.Destination()))
		for _, host := range []string{"localhost", "127.0.0.1"} {
			_, _ = x.f.admin.Exec("DROP USER IF EXISTS '" + childUser(x.job) + "'@'" + host + "'")
		}
	})
}

func assertExecutionAccountLocked(t *testing.T, x *executionFixture) {
	t.Helper()
	var host string
	if err := x.f.admin.QueryRowContext(x.f.ctx, "SELECT Host FROM mysql.user WHERE User = ?", childUser(x.job)).Scan(&host); err != nil {
		t.Fatal("cannot inspect the job-owned import identity")
	}
	var declaration string
	if err := x.f.admin.QueryRowContext(x.f.ctx, "SHOW CREATE USER '"+childUser(x.job)+"'@'"+host+"'").Scan(&declaration); err != nil || !strings.Contains(declaration, "ACCOUNT LOCK") {
		t.Fatal("executor left its positively created import account unlocked")
	}
}

func TestExecutorRestoresNativeTableAndViewBackupWithoutManualImport(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.exec(f.admin, "CREATE VIEW "+qi(f.source)+".local_balances AS SELECT id, credits, note FROM "+qi(f.source)+".players")
	x := newExecutionFixture(f, f.nativeDump())
	registerExecutionCleanup(x)
	before, grantsBefore := f.snapshotSchema(false), backupGrantHash(f, x.settings.Database)
	var phases []restorejob.Phase
	runner := x.runner(restorejob.Options{Observe: func(state restorejob.State) { phases = append(phases, state.Phase) }})
	state, err := runner.Run(f.ctx, x.job)
	if err != nil || state.Phase != restorejob.Succeeded || !state.Pending() {
		var warning *database.RestoreWarningError
		if errors.As(err, &warning) {
			t.Fatalf("complete executor stopped on statement %d warning %d", warning.Statement, warning.Code)
		}
		t.Fatalf("complete executor journey did not succeed: phase=%s problem=%s error=%v", state.Phase, state.Problem, err)
	}
	if f.scalar(f.admin, "SELECT credits FROM "+qi(x.job.Destination())+".local_balances WHERE id=1") != 100 {
		t.Fatal("executor produced an unusable restored view")
	}
	assertExecutionAccountLocked(t, x)
	if f.snapshotSchema(false) != before || backupGrantHash(f, x.settings.Database) != grantsBefore {
		t.Fatal("complete executor changed source data or backup grants")
	}
	if paths, _ := filepath.Glob(filepath.Join(filepath.Dir(x.path), "restore-jobs", ".restore-work-*")); len(paths) != 0 {
		t.Fatal("complete executor retained handled plaintext staging")
	}
	f.exec(f.admin, "UPDATE "+qi(x.job.Destination())+".players SET credits=777 WHERE id=1")
	if err := os.Remove(x.path); err != nil {
		t.Fatal(err)
	}
	replay, err := runner.Run(f.ctx, x.job)
	if err != nil || replay != state || x.gets.Load() != 1 || x.lookups.Load() != 1 || f.scalar(f.admin, "SELECT credits FROM "+qi(x.job.Destination())+".local_balances WHERE id=1") != 777 {
		t.Fatal("terminal duplicate reran SQL, redownloaded, or required local credentials again")
	}
	t.Logf("real executor passed on %s: automatic local key/auth, authenticated download, durable phases %v, scoped import, validation, stopped SQL, locked usable definer, immutable replay", f.engine, phases)
}

func TestExecutorImportFailureAndTimeoutCannotBecomeSuccess(t *testing.T) {
	for _, failure := range []string{"duplicate_row", "statement_timeout"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.tables(f.admin, f.source)
			plain := f.nativeDump()
			if failure == "duplicate_row" {
				plain = append(plain, []byte("INSERT INTO players VALUES (1,0,'duplicate',NULL);\n")...)
			}
			x := newExecutionFixture(f, plain)
			registerExecutionCleanup(x)
			options := restorejob.Options{}
			if failure == "statement_timeout" {
				options.StatementTimeout = time.Nanosecond
			}
			runner := x.runner(options)
			before := f.snapshotSchema(false)
			state, err := runner.Run(f.ctx, x.job)
			if err == nil || state.Phase != restorejob.Failed || !state.WritesStarted {
				t.Fatalf("failed SQL became success: phase=%s problem=%s err=%v", state.Phase, state.Problem, err)
			}
			assertExecutionAccountLocked(t, x)
			if f.snapshotSchema(false) != before {
				t.Fatal("failed executor changed production source")
			}
			replay, err := runner.Run(f.ctx, x.job)
			if err != nil || replay != state || x.gets.Load() != 1 {
				t.Fatal("failed terminal import was repeated")
			}
		})
	}
}

func TestExecutorCommitsTableOnlyDataWhenServerAutocommitIsOff(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	x := newExecutionFixture(f, f.nativeDump())
	registerExecutionCleanup(x)
	previous := f.scalar(f.admin, "SELECT @@global.autocommit")
	f.exec(f.admin, "SET GLOBAL autocommit = 0")
	t.Cleanup(func() {
		if previous != 0 {
			_, _ = f.admin.Exec("SET GLOBAL autocommit = 1")
		}
	})
	state, err := x.runner(restorejob.Options{}).Run(f.ctx, x.job)
	if err != nil || state.Phase != restorejob.Succeeded {
		var warning *database.RestoreWarningError
		if errors.As(err, &warning) {
			t.Fatalf("table-only executor stopped on statement %d warning %d", warning.Statement, warning.Code)
		}
		t.Fatalf("table-only executor failed with inherited autocommit off: phase=%s problem=%s", state.Phase, state.Problem)
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+qi(x.job.Destination())+".players") != 1 {
		t.Fatal("executor validated uncommitted data and rolled it back on close")
	}
}

func TestExecutorNeverReusesAnExistingDestinationOrIdentity(t *testing.T) {
	for _, mode := range []string{"destination", "identity"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.tables(f.admin, f.source)
			x := newExecutionFixture(f, f.nativeDump())
			registerExecutionCleanup(x)
			var account, before string
			if mode == "destination" {
				f.createDatabase(f.admin, x.job.Destination())
				f.exec(f.admin, "CREATE TABLE "+qi(x.job.Destination())+".keep_data (value INT)")
				f.exec(f.admin, "INSERT INTO "+qi(x.job.Destination())+".keep_data VALUES (42)")
			} else {
				authority := openDB(t, x.settings.Restore.User, x.settings.Restore.Password)
				var identity string
				if err := authority.QueryRowContext(f.ctx, "SELECT CURRENT_USER()").Scan(&identity); err != nil {
					t.Fatal("cannot identify fixture authority")
				}
				host := identity[strings.LastIndexByte(identity, '@')+1:]
				account = "'" + childUser(x.job) + "'@'" + host + "'"
				f.exec(f.admin, "CREATE USER "+account+" IDENTIFIED BY 'public_preexisting_identity'")
				before = f.hashQueries([]string{"SHOW CREATE USER " + account})
			}
			state, err := x.runner(restorejob.Options{}).Run(f.ctx, x.job)
			if err == nil || state.Phase != restorejob.Failed || state.Problem != restorejob.UnsafeImport {
				t.Fatalf("existing resource was reused: phase=%s problem=%s err=%v", state.Phase, state.Problem, err)
			}
			if mode == "destination" {
				if state.WritesStarted || f.scalar(f.admin, "SELECT value FROM "+qi(x.job.Destination())+".keep_data") != 42 {
					t.Fatal("existing destination changed")
				}
			} else if before != f.hashQueries([]string{"SHOW CREATE USER " + account}) {
				t.Fatal("pre-existing identity was locked, altered, or replaced without ownership proof")
			}
		})
	}
}

func TestExecutorRechecksRevokedServerAuthorityBeforeWriteIntent(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	x := newExecutionFixture(f, f.nativeDump())
	registerExecutionCleanup(x)
	runner := x.runner(restorejob.Options{Observe: func(state restorejob.State) {
		if state.Phase == restorejob.Verified {
			for _, host := range []string{"localhost", "127.0.0.1"} {
				f.exec(f.admin, "REVOKE CREATE USER ON *.* FROM '"+x.settings.Restore.User+"'@'"+host+"'")
			}
		}
	}})
	state, err := runner.Run(f.ctx, x.job)
	if err == nil || state.Phase != restorejob.Failed || state.Problem != restorejob.WriteAuthorizationUnavailable || state.WritesStarted {
		t.Fatal("executor wrote using stale/revoked provisioning authorization")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", x.job.Destination()) != 0 {
		t.Fatal("revoked authority left a destination")
	}
}

func TestExecutorCancelsAndJoinsBlockedServerSQL(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	x := newExecutionFixture(f, f.nativeDump())
	registerExecutionCleanup(x)
	lockDB := openDB(t, "root", rootPassword)
	locker, err := lockDB.Conn(f.ctx)
	if err != nil {
		t.Fatal("cannot prepare test-owned metadata lock")
	}
	defer locker.Close()
	defer locker.ExecContext(context.Background(), "UNLOCK TABLES")
	locked := make(chan struct{})
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	runner := x.runner(restorejob.Options{Observe: func(state restorejob.State) {
		if state.Phase == restorejob.Validating {
			if _, err := locker.ExecContext(f.ctx, "LOCK TABLES "+qi(x.job.Destination())+".players WRITE"); err != nil {
				t.Error("cannot hold test-owned target lock")
			}
			close(locked)
		}
	}})
	type result struct {
		state restorejob.State
		err   error
	}
	done := make(chan result, 1)
	go func() { state, err := runner.Run(ctx, x.job); done <- result{state, err} }()
	select {
	case <-locked:
	case got := <-done:
		t.Fatalf("executor ended before blocked validation: %v", got.err)
	case <-f.ctx.Done():
		t.Fatal("executor never reached validation")
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER=? AND COMMAND='Query' AND LOWER(STATE) LIKE '%lock%'", childUser(x.job)) == 0 {
		select {
		case <-ticker.C:
		case <-deadline.C:
			cancel()
			t.Fatal("validation query did not wait on the test-owned lock")
		}
	}
	cancel()
	select {
	case got := <-done:
		if got.err == nil || got.state.Phase != restorejob.Canceled || !got.state.WritesStarted {
			t.Fatalf("blocked SQL did not end as a joined cancellation: phase=%s problem=%s err=%v", got.state.Phase, got.state.Problem, got.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("canceled SQL worker or server query did not join")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER=?", childUser(x.job)) != 0 {
		t.Fatal("executor released the job while its server connection was still alive")
	}
	assertExecutionAccountLocked(t, x)
}

func TestExecutorCrashHelper(t *testing.T) {
	phase := restorejob.Phase(os.Getenv("DB_AGENT_EXECUTOR_CRASH_PHASE"))
	if phase == "" {
		return
	}
	requireProofMarker(t)
	settings, err := config.LoadDiagnostic(os.Getenv("DB_AGENT_EXECUTOR_CONFIG"))
	if err != nil {
		t.Fatal("cannot load private child-process fixture configuration")
	}
	data, err := securefile.Read(os.Getenv("DB_AGENT_EXECUTOR_JOB"), 4096)
	var job restorejob.Job
	if err != nil || json.Unmarshal(data, &job) != nil {
		t.Fatal("cannot load local fixture job")
	}
	certificate, err := os.ReadFile(os.Getenv("DB_AGENT_EXECUTOR_CA"))
	pool := x509.NewCertPool()
	if err != nil || !pool.AppendCertsFromPEM(certificate) {
		t.Fatal("cannot load public fixture TLS root")
	}
	client, err := controlplane.New(settings.APIURL, settings.AgentCredential, "0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	options := restorejob.Options{Download: restore.Options{ReserveBytes: 1 << 20, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, Observe: func(state restorejob.State) {
		if state.Phase == phase {
			fmt.Fprintln(os.Stdout, "saved")
			for {
				time.Sleep(time.Hour)
			}
		}
	}}
	// This binding is trusted local fixture enrollment, not copied from delivery.
	binding := restorejob.Binding{DatabaseID: "fixture_database", AgentID: "fixture_agent"}
	runner, err := restorejob.NewRunner(os.Getenv("DB_AGENT_EXECUTOR_CONFIG"), binding, client, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), job); err != nil {
		t.Fatalf("child executor stopped before its crash boundary: %v", err)
	}
	t.Fatal("child executor did not reach the requested crash boundary")
}

func TestExecutorProcessRestartNeverReimportsAnUnknownOutcome(t *testing.T) {
	for _, phase := range []restorejob.Phase{restorejob.Importing, restorejob.Validating} {
		t.Run(string(phase), func(t *testing.T) {
			f := newFixture(t)
			f.tables(f.admin, f.source)
			x := newExecutionFixture(f, f.nativeDump())
			registerExecutionCleanup(x)
			before := f.snapshotSchema(false)
			jobFile := filepath.Join(filepath.Dir(x.path), "job.json")
			data, err := json.Marshal(x.job)
			if err != nil || securefile.Create(jobFile, data) != nil {
				t.Fatal("cannot save child fixture job")
			}
			caFile := filepath.Join(filepath.Dir(x.path), "fixture-ca.pem")
			if err := securefile.Create(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: x.storage.Certificate().Raw})); err != nil {
				t.Fatal("cannot save public fixture root")
			}
			ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecutorCrashHelper$", "-test.timeout=1m")
			child.Env = append(os.Environ(), "DB_AGENT_EXECUTOR_CRASH_PHASE="+string(phase), "DB_AGENT_EXECUTOR_CONFIG="+x.path, "DB_AGENT_EXECUTOR_JOB="+jobFile, "DB_AGENT_EXECUTOR_CA="+caFile)
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if child.ProcessState == nil {
					child.Process.Kill()
					child.Wait()
				}
			}()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != "saved\n" {
				t.Fatal("child did not reach a durable executor crash boundary")
			}
			child.Process.Kill()
			child.Wait()
			runner := x.runner(restorejob.Options{})
			state, err := runner.Run(f.ctx, x.job)
			if err != nil || state.Phase != restorejob.Failed || state.Problem != restorejob.ImportInterrupted || !state.WritesStarted || x.gets.Load() != 1 || x.lookups.Load() != 1 {
				t.Fatal("restart repeated an import or reported success for an unknown outcome")
			}
			if phase == restorejob.Validating && f.scalar(f.admin, "SELECT COUNT(*) FROM "+qi(x.job.Destination())+".players") != 1 {
				t.Fatal("post-import crash lost or repeated committed rows")
			}
			if f.snapshotSchema(false) != before {
				t.Fatal("crashed/replayed executor changed source data")
			}
		})
	}
}
