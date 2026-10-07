package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/securefile"
)

func TestSaveDatabasePreservesSettingsAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "# keep this comment\nagent_credential: dac_existing\napi_url: https://api.example.com\nauto_update: false\nfuture_setting: preserve\nencryption:\n  key_file: local.key\n"
	if err := securefile.Create(path, []byte(original)); err != nil {
		t.Fatal(err)
	}

	database := DatabaseConfig{Driver: "mariadb", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "secret", Databases: []string{"game"}}
	if err := SaveDatabase(path, database); err != nil {
		t.Fatal(err)
	}

	settings, err := LoadDiagnostic(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AgentCredential != "dac_existing" || settings.AutoUpdate || settings.Database.Password != "secret" || settings.Database.Databases[0] != "game" {
		t.Fatal("settings were lost")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "future_setting: preserve") || !strings.Contains(string(contents), "# keep this comment") {
		t.Fatal("unknown settings/comment lost")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("config permissions are not private")
		}
	}

	if err := SaveDatabase(path, database); err != nil {
		t.Fatal(err)
	}

	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatal("staged config was not cleaned up")
	}
}

func TestDiagnosticParserErrorsDoNotExposeValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := securefile.Create(path, nil); err != nil {
		t.Fatal(err)
	}

	for _, data := range []string{"database:\n  port: never-print-this\n", "password: [never-print-this\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}

		_, err := LoadDiagnostic(path)
		if err == nil || strings.Contains(err.Error(), "never-print-this") {
			t.Fatalf("error=%v", err)
		}

		_, err = LoadAgent(path)
		if err == nil || strings.Contains(err.Error(), "never-print-this") {
			t.Fatalf("agent startup error=%v", err)
		}
	}
}

func TestSaveDatabaseRefusesUnconfirmedScope(t *testing.T) {
	database := DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "secret"}
	if err := SaveDatabase(filepath.Join(t.TempDir(), "config.yaml"), database); err == nil {
		t.Fatal("implicit scope was saved")
	}
}
