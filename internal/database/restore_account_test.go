package database

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fivemanage/db-agent/internal/config"
)

func restoreBackupSettings() config.DatabaseConfig {
	return config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "backup-private", Databases: []string{"game_db"}}
}

func TestLocalRestoreAccountSQLRequiresExactHostAndSeparateAccount(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "172.19.0.1", "docker-host.example"} {
		statements, err := LocalRestoreAccountSQL(restoreBackupSettings(), host)
		if err != nil || len(statements) != 3 {
			t.Fatalf("restore plan: %v", err)
		}
		account := "'db_agent_restore'@'" + host + "'"
		if statements[0] != "CREATE USER "+account+" IDENTIFIED BY '<generated password>'" ||
			statements[1] != "GRANT CREATE USER ON *.* TO "+account ||
			statements[2] != "GRANT "+restorePrivileges+" ON "+restoreGrantScope+" TO "+account+" WITH GRANT OPTION" {
			t.Fatal("restore SQL escaped the approved host or namespace")
		}
		if !restoreAuthorityGrants(statements[1:]) {
			t.Fatal("generated grants do not satisfy restore readiness")
		}
	}
	for _, host := range []string{"", "%", "172.19.%", "host' OR '1'='1", "host\nname", "0.0.0.0"} {
		if _, err := LocalRestoreAccountSQL(restoreBackupSettings(), host); err == nil {
			t.Fatal("unsafe client host accepted")
		}
	}
	for _, scenario := range []string{"remote", "reserved", "same_user", "no_scope", "driver"} {
		backup := restoreBackupSettings()
		switch scenario {
		case "remote":
			backup.Host = "remote.example"
		case "reserved":
			backup.Databases = []string{"fm_restore_production"}
		case "same_user":
			backup.User = "db_agent_restore"
		case "no_scope":
			backup.Databases = nil
		case "driver":
			backup.Driver = "postgres"
		}
		if _, err := LocalRestoreAccountSQL(backup, "127.0.0.1"); err == nil {
			t.Fatalf("unsafe backup binding accepted: %s", scenario)
		}
	}
}

func TestProvisionRestoreAccountVerifiesBeforeSaveAndRollsBackOnlyItsOwnAccount(t *testing.T) {
	for _, failure := range []string{"", "create", "first_grant", "second_grant", "verify", "save", "cleanup", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			backup := restoreBackupSettings()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			raw := errors.New("admin-private generated-private raw server error")
			mock.ExpectQuery("SELECT USER").WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("root@172.19.0.1"))
			statements, err := LocalRestoreAccountSQL(backup, "172.19.0.1")
			if err != nil {
				t.Fatal(err)
			}
			for i, statement := range statements {
				pattern := strings.ReplaceAll(regexp.QuoteMeta(statement), regexp.QuoteMeta("<generated password>"), "[0-9a-f]{64}")
				expected := mock.ExpectExec("^" + pattern + "$")
				if failure == "create" && i == 0 || failure == "first_grant" && i == 1 || failure == "second_grant" && i == 2 {
					expected.WillReturnError(raw)
					break
				}
				expected.WillReturnResult(sqlmock.NewResult(0, 0))
			}
			if failure != "" && failure != "create" {
				drop := mock.ExpectExec("^" + regexp.QuoteMeta("DROP USER 'db_agent_restore'@'172.19.0.1'") + "$")
				if failure == "cleanup" {
					drop.WillReturnError(raw)
				} else {
					drop.WillReturnResult(sqlmock.NewResult(0, 0))
				}
			}
			persisted := false
			err = provisionRestoreAccount(ctx, db, backup, "172.19.0.1", func(access config.RestoreConfig) error {
				persisted = true
				if access.ValidateFor(backup) != nil || access.Password == backup.Password || len(access.Password) != 64 {
					t.Fatal("restore account was not separate, generated, and server-bound")
				}
				if failure == "cancel" {
					cancel()
					return ctx.Err()
				}
				if failure != "" {
					return errors.New("private verification or config save failed")
				}
				return nil
			})
			if (err == nil) != (failure == "") || persisted != (failure == "" || failure == "verify" || failure == "save" || failure == "cleanup" || failure == "cancel") {
				t.Fatalf("wrong restore provisioning result: persisted=%t err=%v", persisted, err)
			}
			if err != nil && (strings.Contains(err.Error(), "admin-private") || strings.Contains(err.Error(), "generated-private")) {
				t.Fatal("raw database error leaked")
			}
			if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation not preserved")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProvisionRestoreAccountRejectsChangedHostAndCancellationBeforeSQL(t *testing.T) {
	for _, scenario := range []string{"changed_host", "unsafe_host", "query_error", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancel" {
				cancel()
			} else {
				q := mock.ExpectQuery("SELECT USER")
				switch scenario {
				case "changed_host":
					q.WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("root@172.20.0.1"))
				case "unsafe_host":
					q.WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("root@private'%"))
				case "query_error":
					q.WillReturnError(errors.New("private"))
				}
			}
			err = provisionRestoreAccount(ctx, db, restoreBackupSettings(), "172.19.0.1", func(config.RestoreConfig) error {
				t.Fatal("unsafe host reached persistence")
				return nil
			})
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("host or cancellation check did not fail safely")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProvisionRestoreAccountRefusesDifferentServerBeforeConnecting(t *testing.T) {
	for _, field := range []string{"host", "port", "driver"} {
		backup := restoreBackupSettings()
		admin := backup
		admin.User = "root"
		switch field {
		case "host":
			admin.Host = "::1"
		case "port":
			admin.Port++
		case "driver":
			admin.Driver = "mariadb"
		}
		if err := ProvisionRestoreAccount(context.Background(), admin, backup, "127.0.0.1", nil); err == nil {
			t.Fatal("restore setup accepted a different database server")
		}
	}
}
