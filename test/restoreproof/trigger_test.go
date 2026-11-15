//go:build integration

package restoreproof

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/restorejob"
	"github.com/fivemanage/db-agent/internal/restoreplan"
)

func TestExecutorRestoresNativeTriggersAndViewsWithLockedDefiners(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	s := qi(f.source)
	f.exec(f.admin, "SET SESSION collation_connection=utf8mb4_bin")
	for _, statement := range []string{
		"CREATE VIEW " + s + ".local_balances AS SELECT p.id, p.credits, p.note, UPPER(COALESCE(p.note,'none')) AS label FROM " + s + ".players p WHERE p.credits>=0",
		"CREATE TRIGGER " + s + ".normalize BEFORE INSERT ON " + s + ".players FOR EACH ROW BEGIN IF NEW.credits<0 THEN SET NEW.credits=0; ELSEIF NEW.credits=0 THEN SET NEW.note='zero'; ELSE SET NEW.note=LOWER(CONCAT('POSI','TIVE')); END IF; END",
		"CREATE TRIGGER " + s + ".record_insert AFTER INSERT ON " + s + ".players FOR EACH ROW INSERT INTO " + s + ".audit_log VALUES (NEW.id, 'first')",
		"CREATE TRIGGER " + s + ".second_insert AFTER INSERT ON " + s + ".players FOR EACH ROW INSERT INTO " + s + ".audit_log VALUES (NEW.id, 'second')",
		"CREATE TRIGGER " + s + ".record_update AFTER UPDATE ON " + s + ".players FOR EACH ROW UPDATE " + s + ".audit_log SET action=NEW.note WHERE player_id=OLD.id",
		"CREATE TRIGGER " + s + ".record_delete AFTER DELETE ON " + s + ".players FOR EACH ROW DELETE FROM " + s + ".audit_log WHERE player_id=OLD.id",
	} {
		f.exec(f.admin, statement)
	}
	var queries []string
	for _, name := range []string{"normalize", "record_insert", "second_insert", "record_update", "record_delete"} {
		queries = append(queries, "SHOW CREATE TRIGGER "+s+"."+qi(name))
	}
	queries = append(queries, "SHOW CREATE VIEW "+s+".local_balances")
	before := f.snapshotSchema(false) + f.hashQueries(queries)
	native := f.nativeDump()
	x := newExecutionFixture(f, native)
	registerExecutionCleanup(x)
	grantsBefore := backupGrantHash(f, x.settings.Database)
	if _, err := restoreplan.Prepare(f.ctx, bytes.NewReader(native), restoreplan.Options{
		Driver: f.engine, Source: f.source, Destination: x.job.Destination(),
	}); err != nil {
		t.Fatalf("native trigger/view preflight failed: %v", err)
	}

	state, err := x.runner(restorejob.Options{}).Run(f.ctx, x.job)
	if err != nil || state.Phase != restorejob.Succeeded {
		var warning *database.RestoreWarningError
		if errors.As(err, &warning) {
			t.Fatalf("trigger executor warning on statement %d: %d", warning.Statement, warning.Code)
		}
		t.Fatalf("native trigger/view executor failed: phase=%s problem=%s error=%v", state.Phase, state.Problem, err)
	}
	target := qi(x.job.Destination())
	assertExecutionAccountLocked(t, x)
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".audit_log") != 0 ||
		f.scalar(f.admin, "SELECT credits FROM "+target+".local_balances WHERE id=1") != 100 {
		t.Fatal("triggers fired during data loading or restored view is unusable")
	}
	var identity string
	if err := f.admin.QueryRowContext(f.ctx, "SELECT DEFINER FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=? AND TRIGGER_NAME='record_insert'", x.job.Destination()).Scan(&identity); err != nil ||
		!strings.HasPrefix(identity, childUser(x.job)+"@") {
		t.Fatal("restored trigger retained the source definer")
	}
	f.exec(f.admin, "INSERT INTO "+target+".players VALUES (2,-1,'negative',NULL), (3,0,'empty',NULL), (4,5,'five',NULL)")
	if f.scalar(f.admin, "SELECT credits FROM "+target+".local_balances WHERE id=2") != 0 ||
		f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".players WHERE (id=3 AND note='zero') OR (id=4 AND note='positive')") != 2 ||
		f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".audit_log") != 6 {
		t.Fatal("restored BEFORE/AFTER INSERT trigger or IF branches are unusable")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".local_balances WHERE id=4 AND label='POSITIVE'") != 1 {
		t.Fatal("restored view/trigger scalar functions lost their row references")
	}
	f.exec(f.admin, "UPDATE "+target+".players SET note='changed' WHERE id=2")
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".audit_log WHERE player_id=2 AND action='changed'") != 2 {
		t.Fatal("restored UPDATE trigger still references the source or lost OLD/NEW bindings")
	}
	f.exec(f.admin, "DELETE FROM "+target+".players WHERE id=2")
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".audit_log WHERE player_id=2") != 0 {
		t.Fatal("restored DELETE trigger is unusable")
	}
	if f.snapshotSchema(false)+f.hashQueries(queries) != before || backupGrantHash(f, x.settings.Database) != grantsBefore {
		t.Fatal("trigger restore/use changed source objects, outside data, or backup privileges")
	}
	t.Log("native trigger/view backup -> executor -> validation -> locked definer use passed; triggers did not run during data import and source stayed unchanged")
}

func TestUnsafeNativeTriggersFailBeforeAnyDatabaseWrites(t *testing.T) {
	for _, kind := range []string{"outside_write", "outside_read", "session_variable", "call"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.tables(f.admin, f.source)
			body := "UPDATE " + qi(f.other) + ".sentinel SET value=0 WHERE id=NEW.id"
			switch kind {
			case "outside_read":
				body = "SET NEW.credits=(SELECT value FROM " + qi(f.other) + ".sentinel WHERE id=NEW.id)"
			case "session_variable":
				body = "SET @private=NEW.id"
			case "call":
				body = "CALL " + qi(f.source) + ".unsafe_proc()"
			}
			f.exec(f.admin, "CREATE TRIGGER "+qi(f.source)+".unsafe BEFORE INSERT ON "+qi(f.source)+".players FOR EACH ROW "+body)
			before := f.snapshotSchema(false) + f.hashQueries([]string{"SHOW CREATE TRIGGER " + qi(f.source) + ".unsafe"})
			x := newExecutionFixture(f, f.nativeDump())
			registerExecutionCleanup(x)
			state, err := x.runner(restorejob.Options{}).Run(f.ctx, x.job)
			if !errors.Is(err, restoreplan.ErrUnsupported) || state.Phase != restorejob.Failed || state.WritesStarted {
				t.Fatalf("unsafe native trigger started writes or returned a partial plan: phase=%s error=%v", state.Phase, err)
			}
			if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", x.job.Destination()) != 0 ||
				f.scalar(f.admin, "SELECT COUNT(*) FROM mysql.user WHERE User=?", childUser(x.job)) != 0 ||
				f.snapshotSchema(false)+f.hashQueries([]string{"SHOW CREATE TRIGGER " + qi(f.source) + ".unsafe"}) != before {
				t.Fatal("unsafe trigger preflight created a database/account or changed the source")
			}
		})
	}
}

func TestTriggerBinaryLogPolicyStopsBeforeProvisioning(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.exec(f.admin, "CREATE TRIGGER "+qi(f.source)+".record_insert AFTER INSERT ON "+qi(f.source)+".players FOR EACH ROW INSERT INTO "+qi(f.source)+".audit_log VALUES (NEW.id,'insert')")
	options := planOptions(f)
	plan, err := restoreplan.Prepare(f.ctx, bytes.NewReader(f.nativeDump()), options)
	if err != nil {
		t.Fatal(err)
	}
	backup, access := authorityFixture(f, "")
	var binlog, trusted bool
	if err := f.admin.QueryRowContext(f.ctx, "SELECT @@global.log_bin, @@global.log_bin_trust_function_creators").Scan(&binlog, &trusted); err != nil {
		t.Fatal("cannot inspect disposable binary log policy")
	}
	if !binlog {
		t.Skip("server has no binary log; policy combinations are covered by unit tests")
	}
	f.exec(f.admin, "SET GLOBAL log_bin_trust_function_creators=OFF")
	t.Cleanup(func() { f.exec(f.admin, "SET GLOBAL log_bin_trust_function_creators=?", trusted) })
	called := false
	hook := func(_ context.Context) error { called = true; return nil }
	err = database.ImportRestore(f.ctx, backup, access, plan, 0, hook, hook)
	if !errors.Is(err, database.ErrRestoreTriggerPolicy) || called ||
		f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", plan.Destination()) != 0 {
		t.Fatal("unsafe binary log policy reached provisioning/import")
	}
}
