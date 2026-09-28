package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSaveEnrollmentStoresOnlyIssuedCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db-agent", "config.yaml")
	if err := SaveEnrollment(path, "https://api.example.com", "dac_issued"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "agent_credential: dac_issued") || strings.Contains(string(contents), "enrollment") {
		t.Fatalf("saved config does not contain expected credential")
	}
	settings, err := LoadAgent(path)
	if err != nil || settings.AgentCredential != "dac_issued" {
		t.Fatalf("LoadAgent() credential mismatch: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestSaveEnrollmentNeverOverwritesExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveEnrollment(path, "https://api.example.com", "dac_issued"); err == nil {
		t.Fatal("SaveEnrollment() should reject existing config")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "existing" {
		t.Fatalf("existing config changed: %q, %v", contents, err)
	}
}
