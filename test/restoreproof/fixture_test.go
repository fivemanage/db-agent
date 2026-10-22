//go:build integration

// These tests probe a candidate database privilege boundary, not a production
// SQL remapper. Only scripts/test-restore-proof.sh provisions their target.
// Global-policy probes share a server; this suite must not run in parallel.
package restoreproof

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/dump"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/restore"
	"github.com/fivemanage/db-agent/internal/securefile"
	"github.com/go-sql-driver/mysql"
)

const rootPassword = "restore_proof_root"
const writerPassword = "public_disposable_fixture_password"

type fixture struct {
	t       *testing.T
	ctx     context.Context
	admin   *sql.DB
	source  string
	other   string
	target  string
	user    string
	engine  string
	version string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireProofMarker(t)
	engine := os.Getenv("DB_AGENT_RESTORE_PROOF_ENGINE")
	if engine != "mysql" && engine != "mariadb" {
		t.Fatal("unknown disposable fixture engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	admin := openDB(t, "root", rootPassword)
	var version string
	if err := admin.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatal("cannot reach the disposable SQL fixture")
	}
	if strings.Contains(strings.ToLower(version), "mariadb") != (engine == "mariadb") {
		t.Fatal("disposable SQL fixture engine does not match the requested target")
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal("cannot generate disposable fixture identity")
	}
	prefix := "rp_" + hex.EncodeToString(nonce[:])
	f := &fixture{t: t, ctx: ctx, admin: admin, source: prefix + "_source", other: prefix + "_other", target: prefix + "_target", user: prefix, engine: engine, version: version}
	f.createDatabase(admin, f.source)
	f.createDatabase(admin, f.other)
	f.exec(admin, "CREATE TABLE "+qi(f.other)+".sentinel (id INT PRIMARY KEY, value INT NOT NULL) ENGINE=InnoDB")
	f.exec(admin, "INSERT INTO "+qi(f.other)+".sentinel VALUES (1, 42)")
	return f
}

func requireProofMarker(t *testing.T) {
	t.Helper()
	project := os.Getenv("DB_AGENT_RESTORE_PROOF_ID")
	if project == "" {
		t.Skip("use scripts/test-restore-proof.sh; never point these tests at an existing database")
	}
	marker, err := securefile.Read("/tmp/db-agent-restore-proof", 256)
	if err != nil || string(marker) != project || !strings.HasPrefix(project, "db-agent-restore-proof-") {
		t.Fatal("missing disposable-container marker; refusing database writes")
	}
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (f *fixture) exec(db executor, statement string, args ...any) {
	f.t.Helper()
	if _, err := db.ExecContext(f.ctx, statement, args...); err != nil {
		// SQL/client errors can include the SQL, data, paths, and credentials.
		f.t.Fatalf("fixture statement failed (server error %d)", errorNumber(err))
	}
}

func (f *fixture) createDatabase(db executor, name string) {
	f.t.Helper()
	f.exec(db, "CREATE DATABASE "+qi(name)+" CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
	f.t.Cleanup(func() {
		if _, err := f.admin.Exec("DROP DATABASE " + qi(name)); err != nil {
			f.t.Error("cannot remove a test-owned database from the disposable container")
		}
	})
}

func openDB(t *testing.T, user, password string) *sql.DB {
	t.Helper()
	c := mysql.NewConfig()
	c.User, c.Passwd, c.Net, c.Addr = user, password, "tcp", "127.0.0.1:3306"
	c.Timeout, c.ReadTimeout, c.WriteTimeout = 5*time.Second, 10*time.Second, 10*time.Second
	c.MultiStatements, c.AllowAllFiles, c.InterpolateParams = false, false, false
	c.Logger = log.New(io.Discard, "", 0)
	connector, err := mysql.NewConnector(c)
	if err != nil {
		t.Fatal("cannot configure disposable SQL fixture connection")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (f *fixture) writer() *sql.Conn {
	f.t.Helper()
	grants := "SELECT, INSERT, UPDATE, DELETE, CREATE, REFERENCES, CREATE VIEW, SHOW VIEW, CREATE ROUTINE, EXECUTE, TRIGGER, EVENT"
	for _, host := range []string{"localhost", "127.0.0.1"} {
		account := "'" + f.user + "'@'" + host + "'"
		// Account DDL doesn't accept a password parameter marker on both engines.
		// This literal is a fixed public fixture value, not installation input.
		f.exec(f.admin, "CREATE USER "+account+" IDENTIFIED BY '"+writerPassword+"'")
		f.t.Cleanup(func() {
			if _, err := f.admin.Exec("DROP USER " + account); err != nil {
				f.t.Error("cannot remove a test-owned account from the disposable container")
			}
		})
		// Database-level grants treat _ and % as patterns, including in backticks.
		scope := strings.NewReplacer(`\`, `\\`, "_", `\_`, "%", `\%`).Replace(f.target)
		f.exec(f.admin, "GRANT "+grants+" ON "+qi(scope)+".* TO "+account)
	}
	db := openDB(f.t, f.user, writerPassword)
	conn, err := db.Conn(f.ctx)
	if err != nil {
		f.t.Fatal("cannot authenticate the scoped disposable restore account")
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func qi(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func errorNumber(err error) uint16 {
	var server *mysql.MySQLError
	if errors.As(err, &server) {
		return server.Number
	}
	return 0
}

func permissionDenied(err error) bool {
	switch errorNumber(err) {
	case 1044, 1142, 1143, 1227, 1370:
		return true
	default:
		return false
	}
}

// This is structured, authored fixture DDL, not a rewrite of an existing dump.
// It establishes the candidate's privilege and runtime-object behavior only.
func objectStatements(schema string) []string {
	s := qi(schema)
	return []string{
		"CREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW " + s + ".balances AS SELECT p.id, p.credits, p.note FROM " + s + ".players AS p",
		"CREATE PROCEDURE " + s + ".award(IN wanted INT) SQL SECURITY DEFINER BEGIN UPDATE " + s + ".players SET credits = credits + 1 WHERE id = wanted; INSERT INTO " + s + ".audit_log VALUES (wanted, 'award'); END",
		"CREATE FUNCTION " + s + ".balance(wanted INT) RETURNS INT READS SQL DATA SQL SECURITY DEFINER RETURN (SELECT credits FROM " + s + ".players WHERE id = wanted)",
		"CREATE TRIGGER " + s + ".record_insert AFTER INSERT ON " + s + ".players FOR EACH ROW INSERT INTO " + s + ".audit_log VALUES (NEW.id, 'insert')",
		"CREATE EVENT " + s + ".tick ON SCHEDULE EVERY 1 DAY ON COMPLETION PRESERVE DISABLE DO UPDATE " + s + ".players SET credits = credits + 10 WHERE id = 1",
	}
}

func (f *fixture) tables(db executor, schema string) {
	f.t.Helper()
	s := qi(schema)
	f.exec(db, "CREATE TABLE "+s+".players (id INT PRIMARY KEY, credits INT NOT NULL, note VARCHAR(255), payload VARBINARY(32)) ENGINE=InnoDB")
	f.exec(db, "CREATE TABLE "+s+".audit_log (player_id INT NOT NULL, action VARCHAR(32) NOT NULL) ENGINE=InnoDB")
	f.exec(db, "CREATE TABLE "+s+".items (id INT PRIMARY KEY, owner INT NOT NULL, FOREIGN KEY (owner) REFERENCES "+s+".players(id)) ENGINE=InnoDB")
	// Literal schema names are application data, not identifier-remap targets.
	note := f.source + ".players; quote ' slash \\ newline\n /*!50000 SELECT 1 */"
	f.exec(db, "INSERT INTO "+s+".players VALUES (1, 100, ?, ?)", note, []byte{0, 1, 0xfe, 0xff})
	f.exec(db, "INSERT INTO "+s+".items VALUES (1, 1)")
}

func (f *fixture) objects(db executor, schema string) {
	f.t.Helper()
	for _, statement := range objectStatements(schema) {
		f.exec(db, statement)
	}
}

func (f *fixture) scalar(db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, query string, args ...any) int {
	f.t.Helper()
	var value int
	if err := db.QueryRowContext(f.ctx, query, args...).Scan(&value); err != nil {
		f.t.Fatalf("fixture query failed (server error %d)", errorNumber(err))
	}
	return value
}

func (f *fixture) snapshot() string {
	return f.snapshotSchema(true)
}

func (f *fixture) snapshotSchema(includeObjects bool) string {
	f.t.Helper()
	queries := []string{
		"SELECT id, credits, note, HEX(payload) FROM " + qi(f.source) + ".players ORDER BY id",
		"SELECT id, owner FROM " + qi(f.source) + ".items ORDER BY id",
		"SELECT player_id, action FROM " + qi(f.source) + ".audit_log ORDER BY player_id, action",
		"SELECT id, value FROM " + qi(f.other) + ".sentinel ORDER BY id",
		"SHOW CREATE TABLE " + qi(f.other) + ".sentinel",
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME='profiles'", f.source) != 0 {
		queries = append(queries,
			"SELECT id, role, balance, created_at, note FROM "+qi(f.source)+".profiles ORDER BY id",
			"SHOW CREATE TABLE "+qi(f.source)+".profiles",
		)
	}
	for _, object := range []string{"TABLE players", "TABLE items", "TABLE audit_log", "VIEW balances", "PROCEDURE award", "FUNCTION balance", "TRIGGER record_insert", "EVENT tick"} {
		kind, name, _ := strings.Cut(object, " ")
		if !includeObjects && kind != "TABLE" {
			continue
		}
		queries = append(queries, "SHOW CREATE "+kind+" "+qi(f.source)+"."+qi(name))
	}
	return f.hashQueries(queries)
}

func (f *fixture) hashQueries(queries []string) string {
	f.t.Helper()
	digest := sha256.New()
	for _, query := range queries {
		rows, err := f.admin.QueryContext(f.ctx, query)
		if err != nil {
			f.t.Fatalf("cannot snapshot source fixture (server error %d)", errorNumber(err))
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			f.t.Fatal("cannot inspect source snapshot columns")
		}
		for rows.Next() {
			values := make([]sql.RawBytes, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				f.t.Fatal("cannot read source snapshot")
			}
			for _, value := range values {
				fmt.Fprintf(digest, "%d:", len(value))
				digest.Write(value)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			f.t.Fatal("source snapshot was interrupted")
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func (f *fixture) nativeDump() []byte {
	f.t.Helper()
	settings := config.DatabaseConfig{Driver: f.engine, Host: "127.0.0.1", Port: 3306, User: "root", Password: rootPassword, Databases: []string{f.source}}
	report := database.Inspect(f.ctx, settings)
	if !report.Passed() {
		f.t.Fatal("disposable source database is not dump-ready")
	}
	tool, check := database.DetectTools(f.ctx, report.Driver, report.Version)
	if !check.Passed {
		f.t.Fatal("disposable fixture is missing its native dump client")
	}
	var plain bytes.Buffer
	if err := dump.Run(f.ctx, &plain, settings, tool, dump.Options{Timeout: 20 * time.Second}); err != nil {
		f.t.Fatalf("fixture dump failed: %v", err)
	}
	return plain.Bytes()
}

func (f *fixture) verifiedDump() []byte {
	f.t.Helper()
	plain := f.nativeDump()
	material := sha256.Sum256([]byte("public disposable restore proof key"))
	k, err := key.Parse(key.Prefix + base64.RawURLEncoding.EncodeToString(material[:]))
	if err != nil {
		f.t.Fatal("cannot create public test recovery key")
	}
	dir := f.t.TempDir()
	encrypted := filepath.Join(dir, "backup.dbak")
	file, err := securefile.OpenNew(encrypted)
	if err != nil {
		f.t.Fatal("cannot stage disposable encrypted artifact")
	}
	_, encodeErr := artifact.Encrypt(f.ctx, file, bytes.NewReader(plain), k, artifact.Zstd)
	err = errors.Join(encodeErr, file.Sync(), file.Close())
	if err != nil {
		f.t.Fatal("cannot encrypt disposable dump")
	}
	restorer, err := restore.New(nil, restore.Options{ReserveBytes: 1 << 20})
	if err != nil {
		f.t.Fatal(err)
	}
	output := filepath.Join(dir, "verified.sql")
	if _, err := restorer.Restore(f.ctx, restore.Request{File: encrypted, Output: output, Key: k}); err != nil {
		f.t.Fatalf("fixture artifact recovery failed: %v", err)
	}
	verified, err := securefile.Read(output, 1<<20)
	if err != nil || !bytes.Equal(verified, plain) {
		f.t.Fatal("authenticated fixture SQL differs from the original dump")
	}
	return verified
}

func (f *fixture) nativeImport(user, password, defaultDB string, contents []byte) error {
	f.t.Helper()
	client := "mysql"
	if f.engine == "mariadb" {
		client = "mariadb"
	}
	path, err := exec.LookPath(client)
	if err != nil {
		f.t.Fatal("disposable fixture is missing its native SQL client")
	}
	options := filepath.Join(f.t.TempDir(), "client.cnf")
	// All credentials here are fixed public dummy values, never installation secrets.
	data := []byte(fmt.Sprintf("[client]\nuser=%s\npassword=%s\nhost=127.0.0.1\nport=3306\nprotocol=tcp\n", user, password))
	if err := securefile.Create(options, data); err != nil {
		f.t.Fatal("cannot protect disposable client options")
	}
	command := exec.CommandContext(f.ctx, path, "--defaults-file="+options, "--binary-mode=1", "--skip-reconnect", "--default-character-set=utf8mb4", "--database="+defaultDB)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		name = strings.ToUpper(name)
		if !strings.HasPrefix(name, "MYSQL_") && !strings.HasPrefix(name, "MARIADB_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "MYSQL_TEST_LOGIN_FILE="+filepath.Join(filepath.Dir(options), "no-login.cnf"))
	command.Stdin = bytes.NewReader(contents)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}
