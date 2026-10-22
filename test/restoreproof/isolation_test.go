//go:build integration

package restoreproof

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFixtureVersion(t *testing.T) {
	f := newFixture(t)
	t.Logf("disposable restore proof engine: %s %s", f.engine, f.version)
}

func TestDefaultDatabaseDoesNotRemapAnAuthenticatedDump(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.objects(f.admin, f.source)
	before := f.snapshot()
	verified := f.verifiedDump()
	if f.snapshot() != before {
		t.Fatal("dump or artifact recovery changed source data or objects")
	}
	for _, object := range []string{"CREATE DATABASE", "USE ", "balances", "award", "balance", "record_insert", "tick"} {
		if !bytes.Contains(verified, []byte(object)) {
			t.Fatal("native verified dump omitted a required isolation fixture construct")
		}
	}
	writer := f.writer()
	f.createDatabase(writer, f.target)
	if err := f.nativeImport(f.user, writerPassword, f.target, verified); err == nil {
		t.Fatal("unmapped dump unexpectedly imported under the destination-only account")
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal("scoped import failed before the native client could test SQL privileges")
		}
	}
	if f.snapshot() != before {
		t.Fatal("destination-only account changed source data or objects")
	}

	// This deliberately demonstrates the unsafe fallback, using only synthetic
	// databases inside this test's network-isolated throwaway container.
	f.exec(f.admin, "UPDATE "+qi(f.source)+".players SET credits = 777 WHERE id = 1")
	if err := f.nativeImport("root", rootPassword, f.target, verified); err != nil {
		t.Fatal("cannot exercise the default-database counterexample with the native client")
	}
	if got := f.scalar(f.admin, "SELECT credits FROM "+qi(f.source)+".players WHERE id = 1"); got != 100 {
		t.Fatal("default-database counterexample did not replay the original source schema")
	}
	if got := f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", f.target); got != 0 {
		t.Fatal("default database unexpectedly remapped the original dump")
	}
	t.Log("confirmed: --database does not remap a dump; broad credentials can overwrite its embedded source schema")
}

func TestStructuredDestinationObjectsAreUsableWithoutChangingSource(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.objects(f.admin, f.source)
	before := f.snapshot()
	writer := f.writer()
	f.createDatabase(writer, f.target)
	f.exec(writer, "USE "+qi(f.target))
	f.tables(writer, f.target)
	f.objects(writer, f.target)

	if got := f.scalar(writer, "SELECT credits FROM "+qi(f.target)+".balances WHERE id = 1"); got != 100 {
		t.Fatal("destination view is not usable")
	}
	f.exec(writer, "CALL "+qi(f.target)+".award(1)")
	if got := f.scalar(writer, "SELECT "+qi(f.target)+".balance(1)"); got != 101 {
		t.Fatal("destination routine/function did not use the destination schema")
	}
	f.exec(writer, "INSERT INTO "+qi(f.target)+".players VALUES (2, 50, 'new player', 0x0102)")
	if got := f.scalar(writer, "SELECT COUNT(*) FROM "+qi(f.target)+".audit_log WHERE player_id = 2 AND action = 'insert'"); got != 1 {
		t.Fatal("destination trigger is not usable")
	}
	if _, err := writer.ExecContext(f.ctx, "INSERT INTO "+qi(f.target)+".items VALUES (2, 999)"); errorNumber(err) != 1452 {
		t.Fatal("destination foreign-key constraint was not enforced")
	}
	var referenced string
	if err := f.admin.QueryRowContext(f.ctx, "SELECT REFERENCED_TABLE_SCHEMA FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'items' AND REFERENCED_TABLE_NAME IS NOT NULL", f.target).Scan(&referenced); err != nil || referenced != f.target {
		t.Fatal("destination foreign key still references the original schema")
	}
	var sourceNote, targetNote, targetBinary string
	if err := f.admin.QueryRowContext(f.ctx, "SELECT note FROM "+qi(f.source)+".players WHERE id = 1").Scan(&sourceNote); err != nil {
		t.Fatal("cannot inspect source literal fixture")
	}
	if err := writer.QueryRowContext(f.ctx, "SELECT note, HEX(payload) FROM "+qi(f.target)+".players WHERE id = 1").Scan(&targetNote, &targetBinary); err != nil || targetNote != sourceNote || targetBinary != "0001FEFF" {
		t.Fatal("destination changed schema-like string data or binary data")
	}
	assertDefiners(t, f, writer)
	if got := f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ? AND STATUS = 'DISABLED'", f.target); got != 1 {
		t.Fatal("restored event was not held disabled before deliberate activation")
	}
	f.exec(writer, "ALTER EVENT "+qi(f.target)+".tick ON SCHEDULE AT CURRENT_TIMESTAMP + INTERVAL 2 SECOND ENABLE")
	waitForEvent(t, f, writer)
	if f.snapshot() != before {
		t.Fatal("using restored views, routines, triggers, or events changed the source or outside schema")
	}
	t.Log("destination tables, qualified FK/view/routine/function/trigger/event are usable; source data and object definitions are unchanged")
}

func assertDefiners(t *testing.T, f *fixture, writer *sql.Conn) {
	t.Helper()
	var identity string
	if err := writer.QueryRowContext(f.ctx, "SELECT CURRENT_USER()").Scan(&identity); err != nil {
		t.Fatal("cannot inspect scoped restore principal")
	}
	query := `SELECT DEFINER FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?
UNION ALL SELECT DEFINER FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?
UNION ALL SELECT DEFINER FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?
UNION ALL SELECT DEFINER FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?`
	rows, err := f.admin.QueryContext(f.ctx, query, f.target, f.target, f.target, f.target)
	if err != nil {
		t.Fatal("cannot inspect restored object definers")
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var definer string
		if err := rows.Scan(&definer); err != nil || definer != identity {
			t.Fatal("restored object retained a privileged or original definer")
		}
		count++
	}
	if rows.Err() != nil || count != 5 {
		t.Fatal("restored object inventory is incomplete")
	}
}

func waitForEvent(t *testing.T, f *fixture, writer *sql.Conn) {
	t.Helper()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	ticks := time.NewTicker(100 * time.Millisecond)
	defer ticks.Stop()
	for {
		if got := f.scalar(writer, "SELECT credits FROM "+qi(f.target)+".players WHERE id = 1"); got == 111 {
			return
		}
		select {
		case <-ticks.C:
		case <-deadline.C:
			t.Fatal("destination event did not run as its restricted definer")
		case <-f.ctx.Done():
			t.Fatal("destination event drill was canceled")
		}
	}
}

func TestDestinationOnlyPrincipalDeniesEscapeAttempts(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.objects(f.admin, f.source)
	f.exec(f.admin, "CREATE PROCEDURE "+qi(f.other)+".privileged_escape() SQL SECURITY DEFINER UPDATE "+qi(f.source)+".players SET credits = 0")
	before := f.snapshot()
	writer := f.writer()
	f.createDatabase(writer, f.target)
	f.tables(writer, f.target)
	f.exec(writer, "USE "+qi(f.target))
	source, target, other := qi(f.source), qi(f.target), qi(f.other)
	attacks := []struct {
		name string
		sql  string
	}{
		{"source_read", "SELECT * FROM " + source + ".players"},
		{"source_write", "UPDATE " + source + ".players SET credits = 0"},
		{"other_write", "UPDATE " + other + ".sentinel SET value = 0"},
		{"source_drop", "DROP DATABASE " + source},
		{"source_use", "USE " + source},
		{"outside_definer_call", "CALL " + other + ".privileged_escape()"},
		{"source_function_call", "SELECT " + source + ".balance(1)"},
		{"new_database", "CREATE DATABASE " + qi(f.target+"_escape")},
		{"source_view", "CREATE VIEW " + target + ".escape_view AS SELECT * FROM " + source + ".players"},
		{"source_foreign_key", "CREATE TABLE " + target + ".escape_fk (owner INT, FOREIGN KEY (owner) REFERENCES " + source + ".players(id)) ENGINE=InnoDB"},
		{"global_setting", "SET GLOBAL max_connections = 999"},
		{"user_creation", "CREATE USER 'restore_proof_escape'@'localhost'"},
		{"grant", "GRANT SELECT ON " + source + ".* TO '" + f.user + "'@'localhost'"},
		{"file_write", "SELECT 1 INTO OUTFILE '/tmp/restore-proof-escape'"},
		{"multi_statement", "INSERT INTO " + target + ".audit_log VALUES (1, 'escape'); UPDATE " + source + ".players SET credits = 0"},
		{"client_command", `\! touch /tmp/restore-proof-client-escape`},
		{"source_comment", "/*!50000 UPDATE " + source + ".players SET credits = 0 */"},
		{"privileged_view_definer", "CREATE DEFINER='root'@'localhost' VIEW " + target + ".root_view AS SELECT 1 AS id"},
		{"privileged_routine_definer", "CREATE DEFINER='root'@'localhost' PROCEDURE " + target + ".root_routine() SELECT 1"},
		{"privileged_trigger_definer", "CREATE DEFINER='root'@'localhost' TRIGGER " + target + ".root_trigger AFTER INSERT ON " + target + ".players FOR EACH ROW INSERT INTO " + target + ".audit_log VALUES (NEW.id, 'root')"},
		{"privileged_event_definer", "CREATE DEFINER='root'@'localhost' EVENT " + target + ".root_event ON SCHEDULE EVERY 1 DAY DISABLE DO SELECT 1"},
	}
	if f.engine == "mariadb" {
		attacks = append(attacks, struct{ name, sql string }{"mariadb_comment", "/*M!100100 UPDATE " + source + ".players SET credits = 0 */"})
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			_, err := writer.ExecContext(f.ctx, attack.sql)
			if err == nil {
				t.Fatal("destination-only principal accepted an escape attempt")
			}
			if attack.name == "multi_statement" || attack.name == "client_command" {
				if errorNumber(err) != 1064 {
					t.Fatalf("single-statement SQL protocol did not reject client/multi-statement syntax (server error %d)", errorNumber(err))
				}
			} else if !permissionDenied(err) {
				t.Fatalf("escape attempt failed without proving a privilege denial (server error %d)", errorNumber(err))
			}
		})
	}
	if f.snapshot() != before {
		t.Fatal("escape attempts changed source or outside data/objects")
	}
	if got := f.scalar(writer, "SELECT COUNT(*) FROM "+target+".audit_log"); got != 0 {
		t.Fatal("multi-statement attack partially executed")
	}
}

func TestFreshDestinationAndExactGrantScope(t *testing.T) {
	f := newFixture(t)
	writer := f.writer()
	lookalike := strings.Replace(f.target, "_", "X", 1)
	f.createDatabase(f.admin, lookalike)
	if _, err := writer.ExecContext(f.ctx, "CREATE TABLE "+qi(lookalike)+".escape (id INT)"); err == nil {
		t.Fatal("unescaped grant wildcard widened restore scope")
	}
	f.createDatabase(writer, f.target)
	f.exec(writer, "CREATE TABLE "+qi(f.target)+".sentinel (id INT PRIMARY KEY, value INT)")
	f.exec(writer, "INSERT INTO "+qi(f.target)+".sentinel VALUES (1, 42)")
	if _, err := writer.ExecContext(f.ctx, "CREATE DATABASE "+qi(f.target)); errorNumber(err) != 1007 {
		t.Fatal("replayed destination creation did not reject an existing database")
	}
	if _, err := writer.ExecContext(f.ctx, "DROP TABLE "+qi(f.target)+".sentinel"); err == nil {
		t.Fatal("candidate principal can silently erase an existing restore destination")
	}
	if f.scalar(writer, "SELECT value FROM "+qi(f.target)+".sentinel WHERE id = 1") != 42 {
		t.Fatal("duplicate destination attempt changed existing data")
	}
}

func TestDeferredRoutineBodiesRequirePreflight(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.objects(f.admin, f.source)
	before := f.snapshot()
	writer := f.writer()
	f.createDatabase(writer, f.target)
	source, target := qi(f.source), qi(f.target)
	// Both servers permit creation even though execution is unauthorized.
	f.exec(writer, "CREATE PROCEDURE "+target+".deferred_escape() SQL SECURITY DEFINER UPDATE "+source+".players SET credits = 0")
	if _, err := writer.ExecContext(f.ctx, "CALL "+target+".deferred_escape()"); !permissionDenied(err) {
		t.Fatal("deferred source write was not blocked at execution")
	}
	dynamic := "UPDATE " + source + ".players SET credits = 0"
	f.exec(writer, "CREATE PROCEDURE "+target+".dynamic_escape() SQL SECURITY DEFINER BEGIN SET @rp_sql = '"+dynamic+"'; PREPARE rp_stmt FROM @rp_sql; EXECUTE rp_stmt; DEALLOCATE PREPARE rp_stmt; END")
	if _, err := writer.ExecContext(f.ctx, "CALL "+target+".dynamic_escape()"); !permissionDenied(err) {
		t.Fatal("dynamic source write was not blocked at execution")
	}
	if f.snapshot() != before {
		t.Fatal("deferred or dynamic source reference changed the original database")
	}
	t.Log("confirmed: CREATE success does not validate routine references; unsupported static/dynamic bodies need rejection before writes")
}

func TestInvokerSecurityIsNotASafeDefinerReplacement(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	writer := f.writer()
	f.createDatabase(writer, f.target)
	f.exec(writer, "CREATE PROCEDURE "+qi(f.target)+".invoker_escape() SQL SECURITY INVOKER UPDATE "+qi(f.source)+".players SET credits = 0 WHERE id = 1")
	// A later privileged caller defeats the apparent destination-only boundary.
	// This counterexample mutates only this test's synthetic source database.
	f.exec(f.admin, "CALL "+qi(f.target)+".invoker_escape()")
	if f.scalar(f.admin, "SELECT credits FROM "+qi(f.source)+".players WHERE id = 1") != 0 {
		t.Fatal("invoker-security counterexample did not exercise caller privileges")
	}
	t.Log("confirmed: replacing DEFINER with SQL SECURITY INVOKER can escape through a privileged caller; reject or safely normalize the body/security context")
}

func TestSchemaPrivilegesAreNotACompleteSQLSandbox(t *testing.T) {
	f := newFixture(t)
	writer := f.writer()
	lock := f.user + "_advisory"
	if f.scalar(writer, "SELECT GET_LOCK(?, 0)", lock) != 1 {
		t.Fatal("cannot exercise advisory-lock counterexample")
	}
	if f.scalar(writer, "SELECT RELEASE_LOCK(?)", lock) != 1 {
		t.Fatal("cannot release test-owned advisory lock")
	}
	t.Log("confirmed: schema grants alone still allow connection/global side effects such as GET_LOCK; an expression/statement allowlist is required")
}

func TestRevokedAuthorizationRejectsSubsequentDestinationWrites(t *testing.T) {
	f := newFixture(t)
	writer := f.writer()
	f.createDatabase(writer, f.target)
	f.exec(writer, "CREATE TABLE "+qi(f.target)+".sentinel (id INT PRIMARY KEY)")
	for _, host := range []string{"localhost", "127.0.0.1"} {
		f.exec(f.admin, "REVOKE ALL PRIVILEGES, GRANT OPTION FROM '"+f.user+"'@'"+host+"'")
	}
	// A new connection avoids pretending that every server invalidates cached
	// database-level privileges immediately on an already-selected schema.
	if err := writer.Close(); err != nil {
		t.Fatal("cannot close revoked fixture session")
	}
	fresh := openDB(t, f.user, writerPassword)
	if _, err := fresh.ExecContext(f.ctx, "INSERT INTO "+qi(f.target)+".sentinel VALUES (1)"); !permissionDenied(err) {
		t.Fatal("revoked authorization accepted a subsequent destination write")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+qi(f.target)+".sentinel") != 0 {
		t.Fatal("revoked fixture account committed data")
	}
}

func TestLocalFileReadsRemainDisabledInTheDriver(t *testing.T) {
	f := newFixture(t)
	original := f.scalar(f.admin, "SELECT @@global.local_infile")
	f.exec(f.admin, "SET GLOBAL local_infile = 1")
	t.Cleanup(func() {
		statement := "SET GLOBAL local_infile = 0"
		if original != 0 {
			statement = "SET GLOBAL local_infile = 1"
		}
		if _, err := f.admin.Exec(statement); err != nil {
			t.Error("cannot reset disposable fixture local-infile policy")
		}
	})
	writer := f.writer()
	f.createDatabase(writer, f.target)
	f.exec(writer, "CREATE TABLE "+qi(f.target)+".sentinel (value TEXT)")
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if _, err := writer.ExecContext(ctx, "LOAD DATA LOCAL INFILE '/tmp/db-agent-restore-proof' INTO TABLE "+qi(f.target)+".sentinel"); err == nil || errorNumber(err) != 0 {
		t.Fatalf("unregistered local file was not rejected client-side (server error %d)", errorNumber(err))
	}
	if err := writer.PingContext(f.ctx); err != nil || ctx.Err() != nil {
		t.Fatal("local-file probe failed through cancellation or a broken connection instead of client-side file restrictions")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+qi(f.target)+".sentinel") != 0 {
		t.Fatal("local-file attempt imported file contents")
	}
}

func TestBinaryLogPolicyCanBlockLeastPrivilegeFunctions(t *testing.T) {
	f := newFixture(t)
	if f.scalar(f.admin, "SELECT @@global.log_bin") == 0 {
		t.Skip("fixture has binary logging disabled; the enabled-binlog case is exercised on MySQL")
	}
	writer := f.writer()
	f.createDatabase(writer, f.target)
	f.exec(f.admin, "SET GLOBAL log_bin_trust_function_creators = 0")
	t.Cleanup(func() {
		if _, err := f.admin.Exec("SET GLOBAL log_bin_trust_function_creators = 1"); err != nil {
			t.Error("cannot reset disposable fixture binary-log policy")
		}
	})
	statement := "CREATE FUNCTION " + qi(f.target) + ".no_global_privileges() RETURNS INT DETERMINISTIC RETURN 1"
	if _, err := writer.ExecContext(f.ctx, statement); errorNumber(err) != 1419 {
		t.Fatal("binary-log policy did not expose the expected least-privilege readiness blocker")
	}
	t.Log("confirmed: binary-log function policy requires a readiness failure, not silently granting SUPER or changing a server setting")
}
