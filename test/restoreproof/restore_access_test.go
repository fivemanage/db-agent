//go:build integration

package restoreproof

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/restoreplan"
)

const provisioningPrivileges = "CREATE, INSERT, SELECT, UPDATE, DELETE, REFERENCES, CREATE VIEW, SHOW VIEW"
const provisioningScope = "`fm\\_restore\\_%`.*"

func TestInstallRestoreAccountProvisioningPreservesBackupAndProduction(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	backup, _ := authorityFixture(f, "")
	before, grantsBefore := f.snapshotSchema(false), backupGrantHash(f, backup)
	admin := backup
	admin.User, admin.Password = "root", rootPassword
	report := database.Inspect(f.ctx, admin)
	if report.ClientHost == "" || !report.CanCreateUser {
		t.Fatal("cannot inspect the disposable installer connection")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveEnrollment(path, "https://control.example", "issued-fixture-credential"); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveDatabase(path, backup); err != nil {
		t.Fatal(err)
	}
	var access config.RestoreConfig
	err := database.ProvisionRestoreAccount(f.ctx, admin, backup, report.ClientHost, func(candidate config.RestoreConfig) error {
		if err := database.CheckRestoreAccess(f.ctx, backup, candidate); err != nil {
			return err
		}
		if err := config.SaveRestoreAccess(path, backup, candidate); err != nil {
			return err
		}
		access = candidate
		return nil
	})
	if err != nil {
		t.Fatalf("installer could not provision verified restore access: %v", err)
	}
	t.Cleanup(func() { f.exec(f.admin, "DROP USER 'db_agent_restore'@'"+report.ClientHost+"'") })
	saved, err := config.LoadDiagnostic(path)
	if err != nil || saved.Restore.Password != access.Password || saved.Database.Password != backup.Password || saved.Restore.ValidateFor(backup) != nil {
		t.Fatal("installer did not save separate server-bound credentials")
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte(rootPassword)) {
		t.Fatal("temporary installer administrator credentials were saved")
	}
	authority := openDB(t, access.User, access.Password)
	if _, err := authority.ExecContext(f.ctx, "SELECT * FROM "+qi(f.source)+".players"); !permissionDenied(err) {
		t.Fatal("installed restore authority can read production")
	}
	if err := database.ProvisionRestoreAccount(f.ctx, admin, backup, report.ClientHost, func(config.RestoreConfig) error {
		t.Fatal("existing restore account was overwritten")
		return nil
	}); err == nil {
		t.Fatal("installer accepted an existing restore account")
	}
	if err := database.CheckRestoreAccess(f.ctx, backup, access); err != nil {
		t.Fatal("failed duplicate provisioning changed the existing restore account")
	}
	if f.snapshotSchema(false) != before || backupGrantHash(f, backup) != grantsBefore {
		t.Fatal("installer changed production or backup grants")
	}
}

func TestInstallRestoreAccountIsRemovedWhenPrivateSaveFails(t *testing.T) {
	f := newFixture(t)
	backup, _ := authorityFixture(f, "")
	admin := backup
	admin.User, admin.Password = "root", rootPassword
	report := database.Inspect(f.ctx, admin)
	checked := false
	err := database.ProvisionRestoreAccount(f.ctx, admin, backup, report.ClientHost, func(access config.RestoreConfig) error {
		if err := database.CheckRestoreAccess(f.ctx, backup, access); err != nil {
			return err
		}
		checked = true
		return errors.New("private config save failed")
	})
	if err == nil || !checked {
		t.Fatal("failed save did not reject a newly verified account")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM mysql.user WHERE User = 'db_agent_restore'") != 0 {
		t.Fatal("failed installer attempt left its new restore account behind")
	}
}

func authorityFixture(f *fixture, mode string) (config.DatabaseConfig, config.RestoreConfig) {
	backupUser, authorityUser := f.user+"_backup", f.user+"_setup"
	for _, host := range []string{"localhost", "127.0.0.1"} {
		for _, user := range []string{backupUser, authorityUser} {
			account := "'" + user + "'@'" + host + "'"
			f.exec(f.admin, "CREATE USER "+account+" IDENTIFIED BY '"+writerPassword+"'")
			f.t.Cleanup(func() {
				if _, err := f.admin.Exec("DROP USER " + account); err != nil {
					f.t.Error("cannot remove test-owned restore setup account")
				}
			})
		}
		f.exec(f.admin, "GRANT SELECT, SHOW VIEW ON "+qi(strings.ReplaceAll(f.source, "_", `\_`))+".* TO '"+backupUser+"'@'"+host+"'")
		account := "'" + authorityUser + "'@'" + host + "'"
		global := "GRANT CREATE USER ON *.* TO " + account
		schema := "GRANT " + provisioningPrivileges + " ON " + provisioningScope + " TO " + account + " WITH GRANT OPTION"
		switch mode {
		case "missing_create_user":
			global = ""
		case "broad":
			global = "GRANT ALL PRIVILEGES ON *.* TO " + account
		case "no_delegation":
			schema = strings.TrimSuffix(schema, " WITH GRANT OPTION")
		case "wildcard":
			schema = strings.Replace(schema, provisioningScope, "`fm_restore_%`.*", 1)
		}
		if global != "" {
			f.exec(f.admin, global)
		}
		f.exec(f.admin, schema)
		if mode == "production" {
			f.exec(f.admin, "GRANT SELECT ON "+qi(strings.ReplaceAll(f.source, "_", `\_`))+".* TO "+account)
		}
	}
	backup := config.DatabaseConfig{Driver: f.engine, Host: "127.0.0.1", Port: 3306, User: backupUser, Password: writerPassword, Databases: []string{f.source}}
	access := config.RestoreConfig{Authorized: true, Driver: f.engine, Host: backup.Host, Port: backup.Port, User: authorityUser, Password: writerPassword}
	return backup, access
}

func backupGrantHash(f *fixture, backup config.DatabaseConfig) string {
	return f.hashQueries([]string{
		"SHOW GRANTS FOR '" + backup.User + "'@'localhost'",
		"SHOW GRANTS FOR '" + backup.User + "'@'127.0.0.1'",
	})
}

func TestRestoreAuthorizationCanDelegateAnExactDestinationWithoutChangingBackupAccess(t *testing.T) {
	f := newFixture(t)
	options := planOptions(f)
	f.tables(f.admin, f.source)
	f.exec(f.admin, "CREATE VIEW "+qi(f.source)+".local_balances AS SELECT id, credits FROM "+qi(f.source)+".players")
	backup, access := authorityFixture(f, "")
	before, grantsBefore := f.snapshotSchema(false), backupGrantHash(f, backup)
	if err := database.CheckRestoreAccess(f.ctx, backup, access); err != nil {
		t.Fatalf("explicit direct-grant restore access was not ready: %v", err)
	}
	plan, err := restoreplan.Prepare(f.ctx, bytes.NewReader(f.verifiedDump()), options)
	if err != nil {
		t.Fatalf("cannot preflight authorization fixture: %v", err)
	}
	authority := openDB(t, access.User, access.Password)
	if _, err := authority.ExecContext(f.ctx, "SELECT * FROM "+qi(f.source)+".players"); !permissionDenied(err) {
		t.Fatal("restore provisioning account unexpectedly reads production")
	}
	child := f.user + "_import"
	for _, host := range []string{"localhost", "127.0.0.1"} {
		account := "'" + child + "'@'" + host + "'"
		// Only fixed agent-authored account DDL goes through the authority.
		// The recovered SQL below uses the child principal instead.
		f.exec(authority, "CREATE USER "+account+" IDENTIFIED BY '"+writerPassword+"'")
		t.Cleanup(func() {
			if _, err := f.admin.Exec("DROP USER " + account); err != nil {
				t.Error("cannot remove test-owned delegated import principal")
			}
		})
		f.exec(authority, "GRANT "+provisioningPrivileges+" ON "+qi(strings.ReplaceAll(f.target, "_", `\_`))+".* TO "+account)
	}
	importer := openDB(t, child, writerPassword)
	if err := plan.ForEach(f.ctx, func(statement string) error { _, err := importer.ExecContext(f.ctx, statement); return err }); err != nil {
		t.Fatalf("delegated destination import failed (server error %d)", errorNumber(err))
	}
	t.Cleanup(func() { _, _ = f.admin.Exec("DROP DATABASE " + qi(f.target)) })
	if f.scalar(importer, "SELECT credits FROM "+qi(f.target)+".local_balances WHERE id=1") != 100 {
		t.Fatal("delegated view definer isn't usable")
	}
	if _, err := importer.ExecContext(f.ctx, "UPDATE "+qi(f.source)+".players SET credits=0"); !permissionDenied(err) {
		t.Fatal("delegated import principal can write to production")
	}
	if _, err := importer.ExecContext(f.ctx, "CREATE DATABASE "+qi(f.target+"_outside")); !permissionDenied(err) {
		t.Fatal("delegated import principal escaped its exact destination")
	}
	if f.snapshotSchema(false) != before || backupGrantHash(f, backup) != grantsBefore {
		t.Fatal("restore authorization/delegation/import changed source data or backup grants")
	}
	for _, host := range []string{"localhost", "127.0.0.1"} {
		f.exec(f.admin, "REVOKE CREATE USER ON *.* FROM '"+access.User+"'@'"+host+"'")
	}
	if err := database.CheckRestoreAccess(f.ctx, backup, access); !errors.Is(err, database.ErrRestorePrivileges) {
		t.Fatal("revoked provisioning authority remained ready")
	}
}

func TestUnsafeRestoreAuthorizationIsRejectedWithoutChangingSourceOrBackupGrants(t *testing.T) {
	for _, mode := range []string{"missing_create_user", "broad", "no_delegation", "wildcard", "production"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.tables(f.admin, f.source)
			backup, access := authorityFixture(f, mode)
			before, grantsBefore := f.snapshotSchema(false), backupGrantHash(f, backup)
			if err := database.CheckRestoreAccess(f.ctx, backup, access); !errors.Is(err, database.ErrRestorePrivileges) {
				t.Fatalf("unsafe/incomplete authority accepted: %v", err)
			}
			if f.snapshotSchema(false) != before || backupGrantHash(f, backup) != grantsBefore {
				t.Fatal("read-only restore-access probe modified source data or backup grants")
			}
		})
	}
}
