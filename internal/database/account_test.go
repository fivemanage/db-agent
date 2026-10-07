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

func TestBackupAccountLeastPrivilegeSQL(t *testing.T) {
	for _, test := range []struct{ driver, version, routine string }{
		{"mysql", "5.7.44", "SELECT ON `mysql`.`proc`"},
		{"mysql", "8.0.20", "SHOW_ROUTINE ON *.*"},
		{"mysql", "8.4.0", "SHOW_ROUTINE ON *.*"},
		{"mariadb", "10.3.39", "SELECT ON `mysql`.`proc`"},
		{"mariadb", "11.2.1", "SELECT ON `mysql`.`proc`"},
		{"mariadb", "11.3.0", "SHOW CREATE ROUTINE ON *.*"},
	} {
		t.Run(test.driver+test.version, func(t *testing.T) {
			sql, err := BackupAccountSQL(test.driver, test.version, []string{"game_db", "game_db", "other`%db"})
			if err != nil || len(sql) != 4 {
				t.Fatalf("SQL: %v, error: %v", sql, err)
			}
			if sql[1] != "GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON `game\\_db`.* TO "+backupAccount || sql[2] != "GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON `other``\\%db`.* TO "+backupAccount || sql[3] != "GRANT "+test.routine+" TO "+backupAccount {
				t.Fatalf("unexpected grants: %v", sql)
			}
			if missing := missingGrants(sql[1:], []string{"game_db", "other`%db"}, test.driver); len(missing) != 0 {
				t.Fatalf("generated grants do not satisfy diagnostics: %v", missing)
			}
		})
	}
	if _, err := BackupAccountSQL("mysql", "8.0.19", []string{"game"}); err == nil {
		t.Fatal("requested unavailable least-privilege routine grant")
	}
	for _, scope := range [][]string{nil, {"mysql"}, {"game\nDROP USER"}} {
		if _, err := BackupAccountSQL("mysql", "8.4.0", scope); err == nil {
			t.Fatal("unsafe scope accepted")
		}
	}
}

func TestProvisionAccountPersistenceAndRollback(t *testing.T) {
	for _, failure := range []string{"", "create", "grant", "persist", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			statements, err := BackupAccountSQL("mysql", "8.4.0", []string{"game_db"})
			if err != nil {
				t.Fatal(err)
			}
			rawErr := errors.New("raw-admin-secret and generated-password")
			create := mock.ExpectExec("^CREATE USER 'db_agent_backup'@'localhost' IDENTIFIED BY '[0-9a-f]{64}'$")
			if failure == "create" {
				create.WillReturnError(rawErr)
			} else {
				create.WillReturnResult(sqlmock.NewResult(0, 0))
				for i, statement := range statements[1:] {
					grant := mock.ExpectExec("^" + regexp.QuoteMeta(statement) + "$")
					if failure == "grant" && i == 0 {
						grant.WillReturnError(rawErr)
						break
					}
					grant.WillReturnResult(sqlmock.NewResult(0, 0))
				}
				if failure != "" {
					drop := mock.ExpectExec("^" + regexp.QuoteMeta("DROP USER "+backupAccount) + "$")
					if failure == "cleanup" {
						drop.WillReturnError(rawErr)
					} else {
						drop.WillReturnResult(sqlmock.NewResult(0, 0))
					}
				}
			}
			persisted := false
			admin := config.DatabaseConfig{Host: "127.0.0.1", Port: 3306, User: "root", Password: "raw-admin-secret", Databases: []string{"game_db"}}
			err = provisionAccount(context.Background(), db, admin, "mysql", statements, func(c config.DatabaseConfig) error {
				persisted = true
				if c.User != "db_agent_backup" || c.Password == admin.Password || len(c.Password) != 64 || c.Databases[0] != "game_db" {
					t.Fatal("admin credentials passed to persistence or scope changed")
				}
				if failure == "persist" || failure == "cleanup" {
					return errors.New("private config could not be saved")
				}
				return nil
			})
			if (err == nil) != (failure == "") {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "raw-admin-secret") {
				t.Fatal("raw DB error leaked")
			}
			if persisted != (failure == "" || failure == "persist" || failure == "cleanup") {
				t.Fatal("persistence called after failed SQL")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLocalTCPAccountSQL(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		statements, err := LocalBackupAccountSQL("mysql", "8.4.0", []string{"game_db"}, host)
		if err != nil || len(statements) != 6 {
			t.Fatalf("local plan failed: %v", err)
		}
		ip := host
		if host == "localhost" {
			ip = "127.0.0.1"
		}
		account := "'db_agent_backup'@'" + ip + "'"
		if statements[3] != "CREATE USER "+account+" IDENTIFIED BY '<generated password>'" || statements[4] != "GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON `game\\_db`.* TO "+account {
			t.Fatal("TCP identity not scoped to selected loopback IP and database")
		}
	}
	if _, err := LocalBackupAccountSQL("mysql", "8.4.0", []string{"game"}, "remote.example"); err == nil {
		t.Fatal("remote backup identity accepted")
	}
}

func TestExistingTCPIdentityIsNotAlteredOrRemoved(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements, err := LocalBackupAccountSQL("mysql", "8.4.0", []string{"game"}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("^CREATE USER 'db_agent_backup'@'localhost' IDENTIFIED BY '[0-9a-f]{64}'$").WillReturnResult(sqlmock.NewResult(0, 0))
	for _, statement := range statements[1:3] {
		mock.ExpectExec("^" + regexp.QuoteMeta(statement) + "$").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectExec("^CREATE USER 'db_agent_backup'@'127.0.0.1' IDENTIFIED BY '[0-9a-f]{64}'$").WillReturnError(errors.New("already exists"))
	// The failed TCP CREATE is ambiguous/existing. Only @localhost is ours.
	mock.ExpectExec("^" + regexp.QuoteMeta("DROP USER "+backupAccount) + "$").WillReturnResult(sqlmock.NewResult(0, 0))
	err = provisionAccount(context.Background(), db, config.DatabaseConfig{Host: "127.0.0.1"}, "mysql", statements, func(config.DatabaseConfig) error { t.Fatal("persistence called for failed TCP creation"); return nil })
	if err == nil {
		t.Fatal("existing TCP identity creation accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBothCreatedIdentitiesRollbackWhenVerificationFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements, err := LocalBackupAccountSQL("mysql", "8.4.0", []string{"game"}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if strings.HasPrefix(statement, "CREATE USER ") {
			pattern := strings.ReplaceAll(regexp.QuoteMeta(statement), regexp.QuoteMeta("<generated password>"), "[0-9a-f]{64}")
			mock.ExpectExec("^" + pattern + "$").WillReturnResult(sqlmock.NewResult(0, 0))
		} else {
			mock.ExpectExec("^" + regexp.QuoteMeta(statement) + "$").WillReturnResult(sqlmock.NewResult(0, 0))
		}
	}
	mock.ExpectExec("^" + regexp.QuoteMeta("DROP USER 'db_agent_backup'@'127.0.0.1'") + "$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("^" + regexp.QuoteMeta("DROP USER "+backupAccount) + "$").WillReturnResult(sqlmock.NewResult(0, 0))
	err = provisionAccount(context.Background(), db, config.DatabaseConfig{Host: "127.0.0.1"}, "mysql", statements, func(config.DatabaseConfig) error { return errors.New("verification failed") })
	if err == nil {
		t.Fatal("failed verification accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAdministrativeCredentialsAreConservativelyIdentified(t *testing.T) {
	for _, grant := range []string{
		"GRANT ALL PRIVILEGES ON *.* TO 'admin'@'localhost'",
		"GRANT SYSTEM_VARIABLES_ADMIN ON *.* TO 'admin'@'localhost'",
		"GRANT FILE ON *.* TO 'admin'@'localhost'",
		"GRANT SELECT ON `game`.* TO 'admin'@'localhost' WITH GRANT OPTION",
		"GRANT `admin_role`@`%` TO 'user'@'localhost'",
	} {
		if !administrativeGrants([]string{grant}) {
			t.Fatal("potentially administrative credentials treated as a backup account")
		}
	}
	grants := []string{
		"GRANT USAGE ON *.* TO 'backup'@'localhost'",
		"GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON `game`.* TO 'backup'@'localhost'",
		"GRANT SHOW_ROUTINE ON *.* TO 'backup'@'localhost'",
		"GRANT SELECT ON `mysql`.`proc` TO 'backup'@'localhost'",
	}
	if administrativeGrants(grants) {
		t.Fatal("documented backup privileges classified as administrative")
	}
}
