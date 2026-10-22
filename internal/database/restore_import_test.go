package database

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRestoreImportPrincipalRejectsBroaderOrDelegableGrants(t *testing.T) {
	destination := "fm_restore_" + strings.Repeat("a", 32)
	for _, mode := range []string{"", "namespace", "global", "delegate", "role", "missing"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			grant := "GRANT " + restorePrivileges + " ON " + quoteGrantIdentifier(destination) + ".* TO 'child'@'localhost'"
			switch mode {
			case "namespace":
				grant = "GRANT " + restorePrivileges + " ON " + restoreGrantScope + " TO 'child'@'localhost'"
			case "global":
				grant = "GRANT ALL PRIVILEGES ON *.* TO 'child'@'localhost'"
			case "delegate":
				grant += " WITH GRANT OPTION"
			case "role":
				grant = "GRANT 'unexpanded_role' TO 'child'@'localhost'"
			case "missing":
				grant = strings.Replace(grant, "INSERT, ", "", 1)
			}
			rows := sqlmock.NewRows([]string{"grant"}).AddRow("GRANT USAGE ON *.* TO 'child'@'localhost' IDENTIFIED BY PASSWORD '*PRIVATEHASH'").AddRow(grant)
			mock.ExpectQuery(regexp.QuoteMeta("SHOW GRANTS FOR CURRENT_USER")).WillReturnRows(rows)
			if err := checkRestoreChildGrants(context.Background(), conn, destination); (err == nil) != (mode == "") {
				t.Fatal("child connection was accepted without exact nondelegable destination grants")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestoreAccountLockPolicyIsCheckedBeforeWrites(t *testing.T) {
	for _, test := range []struct {
		driver, version string
		allowed         bool
	}{
		{"mysql", "5.7.5", false}, {"mysql", "5.7.44", true}, {"mysql", "8.4.11", true},
		{"mariadb", "10.3.39-MariaDB", false}, {"mariadb", "10.4.1-MariaDB", false},
		{"mariadb", "10.4.2-MariaDB", true}, {"mariadb", "11.4.13-MariaDB", true},
		{"mysql", "11.4.13-MariaDB", false},
	} {
		if got := supportsRestoreLock(test.driver, test.version); got != test.allowed {
			t.Fatalf("unexpected restore account-lock policy for %s %s", test.driver, test.version)
		}
	}
}

func TestMySQLChecksRequireAnEnforcingServerVersion(t *testing.T) {
	for _, test := range []struct {
		version string
		allowed bool
	}{
		{"5.7.44", false}, {"8.0.15", false}, {"8.0.16", true},
		{"8.4.11", true}, {"9.0.0", true}, {"11.4.13-MariaDB", false},
	} {
		if got := supportsMySQLChecks(test.version); got != test.allowed {
			t.Fatalf("unexpected CHECK enforcement policy for %s", test.version)
		}
	}
}

func TestRestoreDiagnosticsReadWarningCodesWithoutExposingSQL(t *testing.T) {
	for _, code := range []uint16{0, 1265, 1681} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := db.Conn(context.Background())
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		rows := sqlmock.NewRows([]string{"Level", "Code", "Message"})
		if code != 0 {
			rows.AddRow("Warning", code, "private SQL and password data")
		}
		mock.ExpectQuery(regexp.QuoteMeta("SHOW WARNINGS LIMIT 1")).WillReturnRows(rows)
		got, err := restoreWarning(context.Background(), conn)
		conn.Close()
		db.Close()
		if err != nil || got != code {
			t.Fatal("diagnostics did not read the preceding statement's warning")
		}
		if code != 0 && strings.Contains((&RestoreWarningError{Statement: 1, Code: got}).Error(), "private") {
			t.Fatal("warning diagnostics exposed native SQL or credential text")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}
