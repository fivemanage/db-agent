package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
)

const testPassword = "local-db-password-never-reported"

func enrolledConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveEnrollment(path, "http://127.0.0.1:1", "dac_"+strings.Repeat("A", 43)); err != nil {
		t.Fatal(err)
	}
	return path
}

func saveDatabase(t *testing.T, path string) {
	t.Helper()
	err := config.SaveDatabase(path, config.DatabaseConfig{Driver: "mariadb", Host: "127.0.0.1", Port: 1, User: "db_agent_backup", Password: testPassword, Databases: []string{"game"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStatusWaitsForDatabaseBeforeCreatingRecoveryKey(t *testing.T) {
	path := enrolledConfig(t)

	status, err := Status(path)

	if err != nil || status.Database != nil || status.KeyFile != "" || status.Complete {
		t.Fatalf("unconfigured setup reported progress: %+v, %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "encryption.key")); !os.IsNotExist(err) {
		t.Fatal("recovery key was created before a database was saved")
	}
}

func TestStatusBindsRecoveryKeyWithoutReportingSecrets(t *testing.T) {
	path := enrolledConfig(t)
	saveDatabase(t, path)

	first, err := Status(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Status(path)
	if err != nil {
		t.Fatal(err)
	}

	if first.Database == nil || first.Database.Name != "game" || first.Database.User != "db_agent_backup" {
		t.Fatalf("database summary missing: %+v", first.Database)
	}
	if first.KeyFile == "" || first.KeyFile != second.KeyFile || first.KeyAcknowledged || first.Complete {
		t.Fatalf("recovery key was not bound once and left unacknowledged: %+v %+v", first, second)
	}
	settings, err := config.LoadDiagnostic(path)
	if err != nil || settings.Encryption.KeyID == "" || settings.Encryption.KeyAcknowledged {
		t.Fatalf("key binding not saved as unacknowledged: %v", err)
	}
	contents, err := os.ReadFile(first.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(first)
	if strings.Contains(string(encoded), testPassword) || strings.Contains(string(encoded), strings.TrimSpace(string(contents))) {
		t.Fatal("setup status contained a password or key material")
	}
}

func TestFinishLeavesKeyUnacknowledgedWhenChecksFail(t *testing.T) {
	path := enrolledConfig(t)
	saveDatabase(t, path)
	if _, err := Status(path); err != nil {
		t.Fatal(err)
	}

	report := Finish(context.Background(), path)

	if report.Succeeded || len(report.Failed) == 0 {
		t.Fatalf("finish succeeded without a reachable database: %+v", report)
	}
	for _, check := range report.Failed {
		if strings.Contains(check.Message, testPassword) {
			t.Fatal("failed check exposed the database password")
		}
	}
	settings, err := config.LoadDiagnostic(path)
	if err != nil || settings.Encryption.KeyAcknowledged {
		t.Fatalf("failed finish acknowledged the recovery key: %v", err)
	}
}
