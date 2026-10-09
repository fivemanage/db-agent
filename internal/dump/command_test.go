package dump

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/securefile"
)

func testSettings() config.DatabaseConfig {
	return config.DatabaseConfig{Driver: "mysql", Host: "localhost", Port: 3306, User: "backup", Password: "private-test-password", Databases: []string{"game", "second game"}}
}

func testTool(t testing.TB) database.Tool {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return database.Tool{Name: "mysqldump", Path: path, Version: "8.4.2"}
}

func TestCommandHasOnlyScopedSafeFlags(t *testing.T) {
	for _, driver := range []string{"mysql", "mariadb"} {
		t.Run(driver, func(t *testing.T) {
			settings := testSettings()
			settings.Driver = driver
			tool := testTool(t)
			if driver == "mariadb" {
				tool.Name, tool.Version = "mariadb-dump", "11.4.13"
			}
			cmd := buildCommand(settings, tool, filepath.Join(t.TempDir(), "private.cnf"), t.TempDir())
			if !strings.HasPrefix(cmd.args[0], "--defaults-file=") {
				t.Fatal("private defaults file must be the first argument")
			}
			for _, flag := range []string{"--single-transaction", "--quick", "--routines", "--triggers", "--events", "--no-tablespaces", "--skip-lock-tables", "--hex-blob"} {
				if !contains(cmd.args, flag) {
					t.Errorf("missing %s", flag)
				}
			}
			for _, flag := range []string{"--all-databases", "--lock-all-tables", "--lock-tables", "--force", "--flush-logs", "--result-file", "--tab", "--defaults-extra-file"} {
				if contains(cmd.args, flag) {
					t.Errorf("unsafe flag %s", flag)
				}
			}
			if !reflect.DeepEqual(cmd.args[len(cmd.args)-4:], []string{"--databases", "--", "game", "second game"}) {
				t.Fatal("selection changed or option terminator missing")
			}
			if strings.Contains(strings.Join(cmd.args, " ")+strings.Join(cmd.env, " "), settings.Password) {
				t.Fatal("password escaped into process arguments/environment")
			}
			if contains(cmd.args, "--set-gtid-purged=OFF") != (driver == "mysql") || contains(cmd.args, "--column-statistics=0") != (driver == "mysql") {
				t.Fatal("MySQL-only flags applied to the wrong family")
			}
		})
	}
	tool := testTool(t)
	tool.Version = "5.7.44"
	cmd := buildCommand(testSettings(), tool, "/private.cnf", t.TempDir())
	if contains(cmd.args, "--column-statistics=0") || contains(cmd.args, "--get-server-public-key") {
		t.Fatal("MySQL 8-only flags applied to 5.7")
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func TestClientEnvironmentCannotOverrideConnection(t *testing.T) {
	dir := t.TempDir()
	env := clientEnvironment([]string{"PATH=/bin", "MYSQL_PWD=secret", "mysql_home=/unsafe", "MYSQL_TEST_LOGIN_FILE=/old", "MARIADB_HOME=/unsafe", "SYSTEMROOT=C:\\Windows"}, dir)
	want := []string{"PATH=/bin", "SYSTEMROOT=C:\\Windows", "MYSQL_TEST_LOGIN_FILE=" + filepath.Join(dir, "no-login.cnf")}
	if !reflect.DeepEqual(env, want) {
		t.Fatal("ambient client configuration was inherited")
	}
}

func TestPrivateOptionsEscapingAndCleanup(t *testing.T) {
	settings := testSettings()
	settings.User = " backup#\"user "
	settings.Password = " spaces#;\"'\\n\n\r\t\b!include /unsafe "
	parent := t.TempDir()
	path, dir, err := createOptions(settings, parent)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := securefile.Read(path, 4096)
	if err != nil {
		t.Fatal("options file is not private and readable")
	}
	want := "[client]\nuser=\" backup#\\\"user \"\npassword=\" spaces#;\\\"'\\\\n\\n\\r\\t\\b!include /unsafe \"\nhost=\"127.0.0.1\"\nport=3306\nprotocol=tcp\n"
	if !bytes.Equal(contents, []byte(want)) {
		t.Fatal("options quoting did not preserve special characters or prevent line injection")
	}
	if strings.Contains(string(contents), "\n!include") {
		t.Fatal("options directive injection")
	}
	if err := cleanupOptions(path, dir); err != nil {
		t.Fatal(err)
	}
	assertEmptyDir(t, parent)
}

func TestRejectUnsafeScopeAndOptions(t *testing.T) {
	for _, scope := range [][]string{nil, {"mysql"}, {"SYS"}, {"information_schema"}, {""}, {"game\x00--all-databases"}, {"--all-databases"}, {"-B"}, {"game", "game"}, {"game\nsecret"}} {
		settings := testSettings()
		settings.Databases = scope
		if err := validate(settings, testTool(t), Options{}); !errors.Is(err, ErrConfiguration) || strings.Contains(err.Error(), settings.Password) {
			t.Fatal("unsafe scope accepted or credentials exposed")
		}
	}
	for _, host := range []string{"remote.example", "192.0.2.1", "127.0.0.2"} {
		settings := testSettings()
		settings.Host = host
		if err := validate(settings, testTool(t), Options{}); !errors.Is(err, ErrConfiguration) {
			t.Fatal("non-local connection accepted")
		}
	}
	settings := testSettings()
	settings.Password = "secret\x00value"
	if err := validate(settings, testTool(t), Options{}); !errors.Is(err, ErrConfiguration) {
		t.Fatal("NUL password accepted")
	}
	for _, options := range []Options{{Timeout: -time.Second}, {BytesPerSecond: -1}} {
		if err := validate(testSettings(), testTool(t), options); !errors.Is(err, ErrConfiguration) {
			t.Fatal("negative execution options accepted")
		}
	}
	for _, tool := range []database.Tool{{Name: "mariabackup", Path: "/bin/mariabackup", Version: "11.4"}, {Name: "mysqldump", Path: "relative", Version: "8.4"}, {Name: "mysqldump", Path: "/bin/mysqldump"}} {
		if err := validate(testSettings(), tool, Options{}); !errors.Is(err, ErrTool) {
			t.Fatal("undiscovered or physical tool accepted")
		}
	}
}

func assertEmptyDir(t testing.TB, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private options were not cleaned up: entries=%d err=%v", len(entries), err)
	}
}
