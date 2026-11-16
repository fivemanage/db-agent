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

func TestLegacyRestorePermissionsKeepWorkingAndSupplyTheExactUpgradeSQL(t *testing.T) {
	for _, kind := range []string{"tables_and_views", "triggers"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.tables(f.admin, f.source)
			f.exec(f.admin, "CREATE VIEW "+qi(f.source)+".local_balances AS SELECT id, credits FROM "+qi(f.source)+".players")
			if kind == "triggers" {
				f.exec(f.admin, "CREATE TRIGGER "+qi(f.source)+".record_insert AFTER INSERT ON "+qi(f.source)+".players FOR EACH ROW INSERT INTO "+qi(f.source)+".audit_log VALUES (NEW.id,'insert')")
			}
			plain := f.nativeDump()
			x := newExecutionFixture(f, plain)
			registerExecutionCleanup(x)
			for _, host := range []string{"localhost", "127.0.0.1"} {
				f.exec(f.admin, "REVOKE TRIGGER ON "+provisioningScope+" FROM '"+x.settings.Restore.User+"'@'"+host+"'")
			}
			grantQueries := []string{
				"SHOW GRANTS FOR '" + x.settings.Restore.User + "'@'localhost'",
				"SHOW GRANTS FOR '" + x.settings.Restore.User + "'@'127.0.0.1'",
			}
			sourceBefore, backupBefore, authorityBefore := f.snapshotSchema(false), backupGrantHash(f, x.settings.Database), f.hashQueries(grantQueries)

			if err := database.CheckRestoreAccess(f.ctx, x.settings.Database, x.settings.Restore); err != nil {
				t.Fatalf("legacy account no longer passed table/view restore readiness: %v", err)
			}
			err := database.CheckRestoreTriggerAccess(f.ctx, x.settings.Database, x.settings.Restore)
			var missing *database.RestoreTriggerGrantError
			if !errors.As(err, &missing) {
				t.Fatalf("legacy account did not produce trigger upgrade SQL: %v", err)
			}
			authority := openDB(t, x.settings.Restore.User, x.settings.Restore.Password)
			var identity string
			if err := authority.QueryRowContext(f.ctx, "SELECT CURRENT_USER()").Scan(&identity); err != nil {
				t.Fatal("cannot inspect the fixture's matched restore account")
			}
			user, host, ok := strings.Cut(identity, "@")
			want := "GRANT TRIGGER ON " + provisioningScope + " TO '" + user + "'@'" + host + "' WITH GRANT OPTION;"
			if !ok || missing.SQL != want || f.hashQueries(grantQueries) != authorityBefore {
				t.Fatal("upgrade SQL guessed the account host, broadened scope, or executed a grant")
			}

			if kind == "triggers" {
				plan, err := restoreplan.Prepare(f.ctx, bytes.NewReader(plain), restoreplan.Options{
					Driver: f.engine, Source: f.source, Destination: x.job.Destination(),
				})
				if err != nil {
					t.Fatal(err)
				}
				writes := false
				hook := func(context.Context) error { writes = true; return nil }
				err = database.ImportRestore(f.ctx, x.settings.Database, x.settings.Restore, plan, 0, hook, hook)
				if !errors.As(err, &missing) || writes ||
					f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", x.job.Destination()) != 0 ||
					f.scalar(f.admin, "SELECT COUNT(*) FROM mysql.user WHERE User=?", childUser(x.job)) != 0 {
					t.Fatal("trigger restore reached account/database creation without its additional grant")
				}
				f.exec(f.admin, missing.SQL)
				if err := database.CheckRestoreTriggerAccess(f.ctx, x.settings.Database, x.settings.Restore); err != nil {
					t.Fatalf("the displayed SQL did not enable trigger restores: %v", err)
				}
			}

			state, err := x.runner(restorejob.Options{}).Run(f.ctx, x.job)
			if err != nil || state.Phase != restorejob.Succeeded {
				t.Fatalf("legacy account restore failed: phase=%s error=%v", state.Phase, err)
			}
			assertExecutionAccountLocked(t, x)
			target := qi(x.job.Destination())
			if f.scalar(f.admin, "SELECT credits FROM "+target+".local_balances WHERE id=1") != 100 {
				t.Fatal("legacy restore account produced an unusable view")
			}
			if kind == "triggers" {
				f.exec(f.admin, "INSERT INTO "+target+".players VALUES (2,1,'new',NULL)")
				if f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".audit_log WHERE player_id=2") != 1 {
					t.Fatal("trigger was unusable after applying the displayed grant")
				}
			} else if f.hashQueries(grantQueries) != authorityBefore {
				t.Fatal("table/view-only restore silently upgraded its authority's permissions")
			}
			if f.snapshotSchema(false) != sourceBefore || backupGrantHash(f, x.settings.Database) != backupBefore {
				t.Fatal("permission upgrade or restore changed source data or backup grants")
			}
		})
	}
}
