package database

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/go-sql-driver/mysql"
)

func candidate() config.DatabaseConfig {
	return config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "never-print-this", Databases: []string{"game"}}
}

func TestInspectSuccessfulServers(t *testing.T) {
	for _, test := range []struct{ version, driver, routine string }{
		{"8.4.2", "mysql", "GRANT SHOW_ROUTINE ON *.* TO `backup`@`localhost`"},
		{"11.4.8-MariaDB-ubu2404", "mariadb", "GRANT SELECT ON `mysql`.`proc` TO `backup`@`localhost`"},
	} {
		t.Run(test.driver, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectPing()
			mock.ExpectQuery("SELECT VERSION").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(test.version))
			mock.ExpectQuery("SHOW DATABASES").WillReturnRows(sqlmock.NewRows([]string{"Database"}).AddRow("game").AddRow("information_schema").AddRow("mysql"))
			mock.ExpectExec("USE `game`").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("SHOW GRANTS").WillReturnRows(sqlmock.NewRows([]string{"Grants"}).AddRow("GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON `game`.* TO `backup`@`localhost`").AddRow(test.routine))
			r := inspect(context.Background(), db, candidate())
			if !r.Passed() || r.Driver != test.driver || len(r.Databases) != 1 || r.Databases[0] != "game" {
				t.Fatalf("unexpected report: %+v", r)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectConnectionFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		reached bool
		message string
	}{
		{"refused", syscall.ECONNREFUSED, false, "Connection refused"},
		{"timeout", context.DeadlineExceeded, false, "timed out"},
		{"auth", &mysql.MySQLError{Number: 1045, Message: "password never-print-this DSN"}, true, "Authentication rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectPing().WillReturnError(test.err)
			r := inspect(context.Background(), db, candidate())
			index := 0
			if test.reached {
				index = 1
			}
			if r.Passed() || r.Checks[0].Passed != test.reached || !strings.Contains(r.Checks[index].Message, test.message) {
				t.Fatalf("unexpected failure report: %+v", r)
			}
			for _, check := range r.Checks {
				if strings.Contains(check.Message, "never-print-this") || strings.Contains(check.Message, "DSN") {
					t.Fatal("raw error leaked")
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectSelectionAndGrantFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		selected []string
		useErr   error
		want     string
	}{
		{"no implicit scope", nil, nil, "No database selected"},
		{"missing database", []string{"game"}, &mysql.MySQLError{Number: 1049}, "does not exist"},
		{"hidden database", []string{"game"}, &mysql.MySQLError{Number: 1044}, "Insufficient"},
		{"missing grants", []string{"game"}, nil, "TRIGGER"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectPing()
			mock.ExpectQuery("SELECT VERSION").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("8.4.2"))
			mock.ExpectQuery("SHOW DATABASES").WillReturnRows(sqlmock.NewRows([]string{"Database"}).AddRow("game"))
			if len(test.selected) > 0 {
				expected := mock.ExpectExec("USE `game`")
				if test.useErr != nil {
					expected.WillReturnError(test.useErr)
				} else {
					expected.WillReturnResult(sqlmock.NewResult(0, 0))
					mock.ExpectQuery("SHOW GRANTS").WillReturnRows(sqlmock.NewRows([]string{"Grants"}).AddRow("GRANT SELECT ON `game`.* TO `backup`@`localhost`"))
				}
			}
			settings := candidate()
			settings.Databases = test.selected
			r := inspect(context.Background(), db, settings)
			if r.Passed() {
				t.Fatal("unexpected pass")
			}
			found := false
			for _, check := range r.Checks {
				found = found || strings.Contains(check.Message, test.want)
			}
			if !found {
				t.Fatalf("missing remediation %q: %+v", test.want, r)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectUnsupportedServer(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectPing()
	mock.ExpectQuery("SELECT VERSION").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("5.6.51-secret"))
	mock.ExpectQuery("SHOW DATABASES").WillReturnRows(sqlmock.NewRows([]string{"Database"}))
	r := inspect(context.Background(), db, config.DatabaseConfig{})
	if r.Checks[2].Passed || !strings.Contains(r.Checks[2].Message, "Unsupported") {
		t.Fatalf("unexpected report: %+v", r)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectActualUnreachableServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	c := candidate()
	c.Port = port
	r := Inspect(context.Background(), c)
	if r.Passed() || r.Checks[0].Passed {
		t.Fatalf("unreachable DB passed: %+v", r)
	}
}

func TestExplainNeverUsesRawErrors(t *testing.T) {
	for _, err := range []error{errors.New("password never-print-this"), &mysql.MySQLError{Number: 9999, Message: "never-print-this"}, &net.DNSError{Name: "never-print-this"}} {
		if strings.Contains(Explain(err), "never-print-this") {
			t.Fatal("secret leaked")
		}
	}
}

func TestGrantsAreScopedAndConservative(t *testing.T) {
	grants := []string{"GRANT ALL PRIVILEGES ON `game`.* TO `u`@`localhost` IDENTIFIED BY PASSWORD 'secret'", "GRANT SELECT ON `mysql`.`proc` TO `u`@`localhost`"}
	if missing := missingGrants(grants, []string{"game"}, "mariadb"); len(missing) != 0 {
		t.Fatalf("missing: %v", missing)
	}
	if !hasGrant([]string{"GRANT SELECT ON `mysql`.* TO `u`@`localhost`"}, "SELECT", "mysql", "proc") {
		t.Fatal("schema-wide grant did not include its tables")
	}
	if missing := missingGrants(grants, []string{"other"}, "mariadb"); len(missing) == 0 {
		t.Fatal("other schema grants accepted")
	}
	if missing := missingGrants([]string{"GRANT `backup_role` TO `u`@`localhost`"}, []string{"game"}, "mysql"); len(missing) == 0 {
		t.Fatal("unverified role accepted")
	}
}

func TestQuoteIdentifier(t *testing.T) {
	if got := quoteIdentifier("game`db"); got != "`game``db`" {
		t.Fatalf("escaped identifier: %q", got)
	}
}
