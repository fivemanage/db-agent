package database

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/go-sql-driver/mysql"
)

func authorityGrants() []string {
	return []string{
		"GRANT USAGE ON *.* TO 'restore_setup'@'localhost' IDENTIFIED BY PASSWORD '*PRIVATEHASH'",
		"GRANT CREATE USER ON *.* TO 'restore_setup'@'localhost'",
		"GRANT " + restorePrivileges + " ON " + restoreGrantScope + " TO 'restore_setup'@'localhost' WITH GRANT OPTION",
	}
}

func TestRestoreAuthorityRequiresOnlyExplicitScopedDirectGrants(t *testing.T) {
	if !restoreAuthorityGrants(authorityGrants()) {
		t.Fatal("expected narrowly scoped provisioning grants were rejected")
	}
	for _, scenario := range []string{"missing_create_user", "missing_grant_option", "global_all", "source_read", "wildcard_scope", "global_grant_option", "role", "extra_namespace_privilege", "missing_view_privilege"} {
		t.Run(scenario, func(t *testing.T) {
			grants := authorityGrants()
			switch scenario {
			case "missing_create_user":
				grants = append(grants[:1], grants[2:]...)
			case "missing_grant_option":
				grants[2] = strings.TrimSuffix(grants[2], " WITH GRANT OPTION")
			case "global_all":
				grants[1] = "GRANT ALL PRIVILEGES ON *.* TO 'restore_setup'@'localhost'"
			case "source_read":
				grants = append(grants, "GRANT SELECT ON `game`.* TO 'restore_setup'@'localhost'")
			case "wildcard_scope":
				grants[2] = strings.Replace(grants[2], restoreGrantScope, "`fm_restore_%`.*", 1)
			case "global_grant_option":
				grants[1] += " WITH GRANT OPTION"
			case "role":
				grants = append(grants, "GRANT `unexpanded_admin_role` TO 'restore_setup'@'localhost'")
			case "extra_namespace_privilege":
				grants[2] = strings.Replace(grants[2], "GRANT ", "GRANT DROP, ", 1)
			case "missing_view_privilege":
				grants[2] = strings.Replace(grants[2], ", CREATE VIEW", "", 1)
			}
			if restoreAuthorityGrants(grants) {
				t.Fatal("unsafe or insufficient restore authority was accepted")
			}
		})
	}
}

func TestRestoreAccessProbeIsReadOnlyAndSecretSafe(t *testing.T) {
	for _, failure := range []string{"", "authentication", "transport", "identity", "driver", "role", "partial_revokes", "grant_query", "grants"} {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			private := errors.New("private-password PRIVATEHASH raw server response")
			expectRestoreProbe(mock, failure, private)
			err = checkRestoreAccess(context.Background(), db, config.DatabaseConfig{Driver: "mysql", User: "restore_setup"})
			if (err == nil) != (failure == "") {
				t.Fatalf("unexpected readiness result: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), "PRIVATEHASH")) {
				t.Fatal("probe leaked credentials, hashes, or raw server diagnostics")
			}
			if failure == "authentication" && !errors.Is(err, config.ErrRestoreAuthorization) || failure == "transport" && !errors.Is(err, ErrRestoreUnavailable) {
				t.Fatal("authorization and connectivity failures were not distinguished")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func expectRestoreProbe(mock sqlmock.Sqlmock, failure string, private error) {
	ping := mock.ExpectPing()
	if failure == "authentication" {
		ping.WillReturnError(&mysql.MySQLError{Number: 1045, Message: "private-password"})
		return
	}
	if failure == "transport" {
		ping.WillReturnError(private)
		return
	}
	identity, version := "restore_setup@localhost", "8.4.11"
	if failure == "identity" {
		identity = "restore_setup@%"
	}
	if failure == "driver" {
		version = "11.4.13-MariaDB"
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT CURRENT_USER(), VERSION()")).WillReturnRows(sqlmock.NewRows([]string{"identity", "version"}).AddRow(identity, version))
	if failure == "identity" || failure == "driver" {
		return
	}
	role := "NONE"
	if failure == "role" {
		role = "unexpanded_role"
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT CURRENT_ROLE()")).WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(role))
	if failure == "role" {
		return
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT @@global.partial_revokes")).WillReturnRows(sqlmock.NewRows([]string{"partial_revokes"}).AddRow(failure == "partial_revokes"))
	if failure == "partial_revokes" {
		return
	}
	query := mock.ExpectQuery("SHOW GRANTS FOR CURRENT_USER")
	if failure == "grant_query" {
		query.WillReturnError(private)
		return
	}
	grants := authorityGrants()
	if failure == "grants" {
		grants = grants[:1]
	}
	rows := sqlmock.NewRows([]string{"grant"})
	for _, grant := range grants {
		rows.AddRow(grant)
	}
	query.WillReturnRows(rows)
}

func TestCanceledRestoreProbeDoesNotConnect(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkRestoreAccess(ctx, db, config.DatabaseConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled restore probe continued")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
