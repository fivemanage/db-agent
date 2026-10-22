package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/enrollment"
	"github.com/spf13/cobra"
)

func TestEnrollmentResumesWithoutAnotherTokenOrReplacingSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveEnrollment(path, "https://api.example.com", "issued-credential"); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(filepath.Dir(path), "encryption.key")
	if err := os.WriteFile(keyPath, []byte("existing-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	original = append(original, []byte("encryption:\n  key_file: "+keyPath+"\ndatabase:\n  user: backup\n  password: existing-db-secret\n  databases: [game]\n")...)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := ensureEnrollment(context.Background(), path, "", func() (string, error) {
		t.Fatal("resume requested another token")
		return "", nil
	}, func(context.Context, string, string, string) (string, error) {
		t.Fatal("resume attempted registration")
		return "", nil
	})
	if err != nil || settings.AgentCredential != "issued-credential" {
		t.Fatalf("resume failed: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(original) {
		t.Fatal("existing enrollment changed")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil || string(key) != "existing-key-material" {
		t.Fatal("existing key changed")
	}
	if _, err := ensureEnrollment(context.Background(), path, "https://other.example.com", nil, nil); err == nil {
		t.Fatal("API binding changed")
	}
}

func TestEnrollmentSavesIssuedCredentialOnlyAndSurvivesDBSetupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	settings, err := ensureEnrollment(context.Background(), path, "https://api.example.com", func() (string, error) { return "one-time-token", nil }, func(_ context.Context, apiURL, token, _ string) (string, error) {
		if token != "one-time-token" || apiURL != "https://api.example.com" {
			t.Fatal("unexpected registration input")
		}
		return "issued-credential", nil
	})
	if err != nil || settings.AgentCredential != "issued-credential" {
		t.Fatalf("enrollment failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "agent_credential: issued-credential\napi_url: https://api.example.com\n" {
		t.Fatal("config did not contain only issued enrollment settings")
	}
	// No DB settings need to exist for the next invocation to resume.
	if _, err := ensureEnrollment(context.Background(), path, "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentFailureDoesNotWriteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	_, err := ensureEnrollment(context.Background(), path, "https://api.example.com", func() (string, error) { return "token", nil }, func(context.Context, string, string, string) (string, error) { return "", enrollment.ErrInvalidToken })
	if !errors.Is(err, enrollment.ErrInvalidToken) {
		t.Fatalf("error: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed enrollment wrote config")
	}
}

func TestInstallShowsOnlyEnrollmentStageOnEarlyFailure(t *testing.T) {
	previousPath, previousURL, previousServiceOnly := configPath, installAPIURL, installServiceOnly
	configPath = filepath.Join(t.TempDir(), "config.yaml")
	installAPIURL, installServiceOnly = "://invalid", false
	t.Cleanup(func() {
		configPath, installAPIURL, installServiceOnly = previousPath, previousURL, previousServiceOnly
	})
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(&output)

	err := installCmd.RunE(command, nil)

	if err == nil {
		t.Fatal("invalid API URL accepted")
	}
	text := output.String()
	for _, want := range []string{"FiveManage / Database backup setup", "[1/5] Connect to FiveManage", "[FAIL] Enrollment could not be completed."} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing setup feedback %q", want)
		}
	}
	for _, unwanted := range []string{"[2/5]", "[3/5]", "[4/5]", "[5/5]", "Setup complete", "[OK]", "\x1b"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("early failure emitted %q", unwanted)
		}
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("early setup failure wrote a config")
	}
}
