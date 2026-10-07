//go:build integration

package database

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
)

// Run this test inside each isolated fixture container: the generated account
// is intentionally @localhost, not accessible through a Docker bridge address.
func TestProvisionedLocalAccountCanDump(t *testing.T) {
	if os.Getenv("DB_AGENT_PROVISION_INTEGRATION") != "1" {
		t.Skip("run the static test binary inside a diagnostic fixture with DB_AGENT_PROVISION_INTEGRATION=1")
	}
	ctx := context.Background()
	admin := config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "root", Password: "diagnostics_root", Databases: []string{"diagnostic_game"}}
	r := Inspect(ctx, admin)
	if !r.Passed() || !r.CanCreateUser {
		t.Fatalf("fixture admin checks failed: %+v", r)
	}
	db, err := openDatabase(admin)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// This schema differs only where the selected name contains a wildcard.
	if _, err := db.ExecContext(ctx, "CREATE DATABASE diagnosticXgame"); err != nil {
		t.Fatal("cannot create isolated grant-scope fixture")
	}
	defer db.ExecContext(ctx, "DROP DATABASE diagnosticXgame")
	for _, statement := range []string{
		"CREATE VIEW diagnostic_game.setup_view AS SELECT id FROM diagnostic_game.players",
		"CREATE PROCEDURE diagnostic_game.setup_routine() SELECT 1",
		"CREATE TRIGGER diagnostic_game.setup_trigger BEFORE INSERT ON diagnostic_game.players FOR EACH ROW SET NEW.name = NEW.name",
		"CREATE EVENT diagnostic_game.setup_event ON SCHEDULE EVERY 1 DAY DO SELECT 1",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal("cannot create isolated dump objects")
		}
	}
	defer db.ExecContext(ctx, "DROP VIEW diagnostic_game.setup_view")
	defer db.ExecContext(ctx, "DROP PROCEDURE diagnostic_game.setup_routine")
	defer db.ExecContext(ctx, "DROP TRIGGER diagnostic_game.setup_trigger")
	defer db.ExecContext(ctx, "DROP EVENT diagnostic_game.setup_event")
	var backup config.DatabaseConfig
	err = ProvisionBackupAccount(ctx, admin, r.Driver, r.Version, func(c config.DatabaseConfig) error {
		verified := Inspect(ctx, c)
		if !verified.Passed() || verified.Administrative {
			return fmt.Errorf("generated-account diagnostics failed: %+v", verified.Checks)
		}
		backup = c
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(ctx, "DROP USER "+backupAccount)
	defer db.ExecContext(ctx, "DROP USER 'db_agent_backup'@'127.0.0.1'")
	// Rerunning creation must not change the existing account or invoke save.
	if err := ProvisionBackupAccount(ctx, admin, r.Driver, r.Version, func(config.DatabaseConfig) error { t.Fatal("existing account was replaced"); return nil }); err == nil {
		t.Fatal("existing account creation accepted")
	}
	if !Inspect(ctx, backup).Passed() {
		t.Fatal("existing account password was rotated")
	}
	outsideScope := backup
	outsideScope.Databases = []string{"diagnosticXgame"}
	if Inspect(ctx, outsideScope).Passed() {
		t.Fatal("wildcard schema grant leaked access")
	}
	tool := "mysqldump"
	if r.Driver == "mariadb" {
		tool = "mariadb-dump"
	}
	options := filepath.Join(t.TempDir(), "client.cnf")
	contents := fmt.Sprintf("[client]\nuser=%s\npassword=%s\nhost=127.0.0.1\nport=3306\nprotocol=tcp\n", backup.User, backup.Password)
	if err := os.WriteFile(options, []byte(contents), 0o600); err != nil {
		t.Fatal("cannot write private test client options")
	}
	dump := exec.CommandContext(ctx, tool, "--defaults-extra-file="+options, "--single-transaction", "--quick", "--routines", "--triggers", "--events", "--no-tablespaces", "--databases", "diagnostic_game")
	var output bytes.Buffer
	dump.Stdout = &output
	if err := dump.Run(); err != nil {
		t.Fatalf("generated account cannot execute a logical dump: %v", err)
	}
	for _, object := range []string{"players", "setup_view", "setup_routine", "setup_trigger", "setup_event"} {
		if !strings.Contains(output.String(), object) {
			t.Fatalf("dump omitted object %s", object)
		}
	}
}

func TestLocalDatabaseFixtures(t *testing.T) {
	if os.Getenv("DB_AGENT_INTEGRATION") != "1" {
		t.Skip("start docker-compose.diagnostics.yml and set DB_AGENT_INTEGRATION=1")
	}
	for _, server := range []struct {
		driver, env string
		port        int
	}{
		{"mariadb", "DB_AGENT_MARIADB_PORT", 13306},
		{"mysql", "DB_AGENT_MYSQL_PORT", 13307},
	} {
		t.Run(server.driver, func(t *testing.T) {
			port := server.port
			if value := os.Getenv(server.env); value != "" {
				var err error
				port, err = strconv.Atoi(value)
				if err != nil {
					t.Fatal(err)
				}
			}
			base := config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: port, User: "diagnostic_backup", Password: "diagnostics_backup", Databases: []string{"diagnostic_game"}}
			t.Run("success", func(t *testing.T) {
				r := Inspect(context.Background(), base)
				if !r.Passed() || r.Driver != server.driver || len(r.Databases) != 1 || r.Databases[0] != "diagnostic_game" {
					t.Fatalf("report: %+v", r)
				}
			})
			for _, test := range []struct{ name, user, password, schema, want string }{
				{"invalid password", "diagnostic_backup", "wrong-secret-never-print", "diagnostic_game", "Authentication rejected"},
				{"insufficient grants", "diagnostic_readonly", "diagnostics_readonly", "diagnostic_game", "TRIGGER"},
				{"missing database", "diagnostic_backup", "diagnostics_backup", "missing_fixture_database", "does not exist"},
				{"hidden database", "diagnostic_backup", "diagnostics_backup", "diagnostic_hidden", "Insufficient"},
			} {
				t.Run(test.name, func(t *testing.T) {
					c := base
					c.User, c.Password, c.Databases = test.user, test.password, []string{test.schema}
					r := Inspect(context.Background(), c)
					found := false
					for _, check := range r.Checks {
						found = found || strings.Contains(check.Message, test.want)
						if strings.Contains(check.Message, test.password) {
							t.Fatal("password leaked")
						}
					}
					if r.Passed() || !found {
						t.Fatalf("report: %+v", r)
					}
				})
			}
		})
	}
}
