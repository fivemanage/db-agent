package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/securefile"
	"gopkg.in/yaml.v3"
)

func TestKeyBindingPreservesConfigAndCannotDowngradeOrRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveEnrollment(path, "https://api.example.com", "dac_existing"); err != nil {
		t.Fatal(err)
	}
	database := DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "private-db-secret", Databases: []string{"game"}}
	if err := SaveDatabase(path, database); err != nil {
		t.Fatal(err)
	}
	if err := updateDocument(path, func(root *yaml.Node) error { return setYAML(root, "future_setting", "preserve") }); err != nil {
		t.Fatal(err)
	}
	keyPath, err := ResolveKeyPath(path, "")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	if err := SaveKeyBinding(path, keyPath, id, true); err != nil {
		t.Fatal(err)
	}
	if err := SaveKeyBinding(path, keyPath, id, false); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadDiagnostic(path)
	if err != nil || !settings.Encryption.KeyAcknowledged || settings.Encryption.KeyID != id || settings.AgentCredential != "dac_existing" || settings.Database.Password != database.Password {
		t.Fatal("key binding lost local settings or acknowledgement")
	}
	before, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(before), "future_setting: preserve") {
		t.Fatal("unknown setting lost")
	}
	for _, candidate := range []struct{ path, id string }{
		{keyPath, strings.Repeat("b", 64)}, {keyPath + ".other", id},
	} {
		if err := SaveKeyBinding(path, candidate.path, candidate.id, true); err == nil {
			t.Fatal("existing binding rotated")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("failed key binding changed usable config")
		}
	}
}

func TestKeyPathUsesConfigDirectoryAndNeverConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, relative := range []string{"", "encryption.key"} {
		resolved, err := ResolveKeyPath(path, relative)
		if err != nil || resolved != filepath.Join(filepath.Dir(path), "encryption.key") {
			t.Fatalf("key path depends on shell cwd: %v", err)
		}
	}
	for _, invalid := range []string{path, path + ".enrollment-pending"} {
		if _, err := ResolveKeyPath(path, invalid); err == nil {
			t.Fatal("key path aliases private config material")
		}
	}
}

func TestConfigParserErrorsNeverEchoSecrets(t *testing.T) {
	for _, contents := range []string{"password: [private-parser-secret\n", "database:\n  port: private-parser-secret\n"} {
		if _, err := LoadReader(strings.NewReader(contents)); err == nil || strings.Contains(err.Error(), "private-parser-secret") {
			t.Fatal("reader parser error leaked secret")
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := securefile.Create(path, []byte(contents)); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || strings.Contains(err.Error(), "private-parser-secret") {
			t.Fatal("file parser error leaked secret")
		}
	}
}

func TestFailedConfigReplacementPreservesPreviousUsableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveEnrollment(path, "https://api.example.com", "dac_previous"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("simulated filesystem write failure")
	err = updateDocumentWith(path, func(root *yaml.Node) error {
		return setYAML(root, "encryption", EncryptionConfig{KeyFile: "private.key", KeyID: strings.Repeat("a", 64)})
	}, func(staged, target string) error {
		if _, err := os.Stat(staged); err != nil || target != path {
			t.Fatal("replacement did not receive a complete staged file")
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("filesystem failure lost: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed save replaced previous usable config")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed config save leaked secret staging data")
	}
}
