//go:build integration

package dump

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/key"
)

// Execute this static test binary inside isolated diagnostic fixture containers,
// where the matching client is installed and the database is genuinely local.
func TestLogicalDumpFixture(t *testing.T) {
	if os.Getenv("DB_AGENT_DUMP_INTEGRATION") != "1" {
		t.Skip("run inside isolated diagnostic fixtures with DB_AGENT_DUMP_INTEGRATION=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin := config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "root", Password: "diagnostics_root", Databases: []string{"diagnostic_game"}}
	report := database.Inspect(ctx, admin)
	if !report.Passed() {
		t.Fatal("isolated fixture diagnostics failed")
	}
	admin.Driver = report.Driver
	tool, check := database.DetectTools(ctx, report.Driver, report.Version)
	if !check.Passed {
		t.Fatal(check.Message)
	}
	db := fixtureConnection(t, admin)
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() {
		for _, statement := range []string{
			"DROP VIEW diagnostic_game.dump_view", "DROP PROCEDURE diagnostic_game.dump_routine",
			"DROP TRIGGER diagnostic_game.dump_trigger", "DROP EVENT diagnostic_game.dump_event",
			"DROP TABLE diagnostic_game.dump_binary", "DROP USER 'dump_fixture_backup'@'%'",
		} {
			_, _ = db.Exec(statement)
		}
	})
	statements := []string{
		"CREATE TABLE diagnostic_game.dump_binary (id INT PRIMARY KEY, payload BLOB) ENGINE=InnoDB",
		"INSERT INTO diagnostic_game.dump_binary VALUES (1, 0x0001FEFF)",
		"CREATE VIEW diagnostic_game.dump_view AS SELECT id FROM diagnostic_game.players",
		"CREATE PROCEDURE diagnostic_game.dump_routine() SELECT 1",
		"CREATE TRIGGER diagnostic_game.dump_trigger BEFORE INSERT ON diagnostic_game.players FOR EACH ROW SET NEW.name = NEW.name",
		"CREATE EVENT diagnostic_game.dump_event ON SCHEDULE EVERY 1 DAY DO SELECT 1",
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal("cannot create isolated dump objects")
		}
	}
	// The dummy password exercises option-file escaping against the real parser.
	settings := admin
	settings.User, settings.Password = "dump_fixture_backup", "  quotes\"'\\#;\nline\t  "
	if _, err := db.ExecContext(ctx, "CREATE USER 'dump_fixture_backup'@'%' IDENTIFIED BY ?", settings.Password); err != nil {
		t.Fatal("cannot create isolated scoped dump account")
	}
	grant := "GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON diagnostic_game.* TO 'dump_fixture_backup'@'%'"
	if _, err := db.ExecContext(ctx, grant); err != nil {
		t.Fatal("cannot grant fixture schema access")
	}
	grant = "GRANT SHOW_ROUTINE ON *.* TO 'dump_fixture_backup'@'%'"
	if settings.Driver == "mariadb" {
		grant = "GRANT SELECT ON mysql.proc TO 'dump_fixture_backup'@'%'"
	}
	if _, err := db.ExecContext(ctx, grant); err != nil {
		t.Fatal("cannot grant fixture routine visibility")
	}

	for _, compression := range []artifact.Compression{artifact.Zstd, artifact.Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			parent := t.TempDir()
			done := make(chan error, 1)
			go func() {
				err := Run(ctx, writer, settings, tool, Options{TempDir: parent, Timeout: 15 * time.Second, ThrottleIO: true})
				_ = writer.CloseWithError(err)
				done <- err
			}()
			material := sha256.Sum256([]byte("public dummy integration recovery key"))
			recoveryKey, err := key.Parse(key.Prefix + base64.RawURLEncoding.EncodeToString(material[:]))
			if err != nil {
				t.Fatal(err)
			}
			var encrypted, restored bytes.Buffer
			metadata, err := artifact.Encrypt(ctx, &encrypted, reader, recoveryKey, compression)
			_ = reader.CloseWithError(err)
			dumpErr := <-done
			if err != nil || dumpErr != nil {
				t.Fatalf("dump=%v encryption=%v", dumpErr, err)
			}
			decoded, err := artifact.Decrypt(ctx, &restored, bytes.NewReader(encrypted.Bytes()), recoveryKey)
			if err != nil || decoded != metadata {
				t.Fatalf("restore: %v", err)
			}
			for _, object := range []string{"players", "dump_binary", "dump_view", "dump_routine", "dump_trigger", "dump_event", "0x0001FEFF"} {
				if !strings.Contains(restored.String(), object) {
					t.Fatalf("dump omitted fixture object %s", object)
				}
			}
			if strings.Contains(restored.String(), "diagnostic_hidden") {
				t.Fatal("dump expanded outside the selected database")
			}
			assertEmptyDir(t, parent)
		})
	}
	t.Run("authentication failure", func(t *testing.T) {
		bad := settings
		bad.Password = "incorrect-private-test-password"
		parent := t.TempDir()
		err := Run(ctx, io.Discard, bad, tool, Options{TempDir: parent})
		if !errors.Is(err, ErrAuthentication) || strings.Contains(err.Error(), bad.Password) {
			t.Fatalf("authentication error: %v", err)
		}
		assertEmptyDir(t, parent)
	})
	t.Run("hidden database", func(t *testing.T) {
		bad := settings
		bad.Databases = []string{"diagnostic_hidden"}
		parent := t.TempDir()
		err := Run(ctx, io.Discard, bad, tool, Options{TempDir: parent})
		if !errors.Is(err, ErrPrivileges) {
			t.Fatalf("scope error: %v", err)
		}
		assertEmptyDir(t, parent)
	})
}

func fixtureConnection(t *testing.T, settings config.DatabaseConfig) *sql.DB {
	t.Helper()
	c := mysql.NewConfig()
	c.User, c.Passwd, c.Net, c.Addr = settings.User, settings.Password, "tcp", "127.0.0.1:3306"
	c.Logger = log.New(io.Discard, "", 0)
	c.InterpolateParams = true
	c.Timeout, c.ReadTimeout, c.WriteTimeout = 5*time.Second, 5*time.Second, 5*time.Second
	connector, err := mysql.NewConnector(c)
	if err != nil {
		t.Fatal("cannot configure fixture connection")
	}
	return sql.OpenDB(connector)
}
