package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/enrollment"
	"github.com/spf13/cobra"
)

func TestSetupTokenEnvironmentIsConsumedBeforeEnrollmentAndChildren(t *testing.T) {
	const token = "one-time-setup-token"
	t.Setenv(setupTokenEnvironment, "  "+token+"\n")

	provided, err := consumeSetupToken()
	if err != nil || provided != token {
		t.Fatal("setup token was not consumed")
	}
	if _, exists := os.LookupEnv(setupTokenEnvironment); exists {
		t.Fatal("setup token remained in the process environment")
	}
	for _, value := range exec.Command("unused-test-command").Environ() {
		if strings.HasPrefix(value, setupTokenEnvironment+"=") {
			t.Fatal("child process would inherit setup token")
		}
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	settings, err := ensureEnrollment(context.Background(), path, "https://api.example.com", func() (string, error) {
		return provided, nil
	}, func(_ context.Context, _, supplied, _ string) (string, error) {
		if supplied != token || os.Getenv(setupTokenEnvironment) != "" {
			t.Fatal("enrollment did not exclusively receive the setup token")
		}
		return "issued-agent-credential", nil
	})
	if err != nil || settings.AgentCredential != "issued-agent-credential" {
		t.Fatalf("enrollment failed: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(contents), token) || strings.Contains(string(contents), setupTokenEnvironment) {
		t.Fatal("setup token was persisted")
	}
	second, err := consumeSetupToken()
	if err != nil || second != "" {
		t.Fatal("setup token was available a second time")
	}
}

func TestInstallClearsProvidedTokenEvenBeforeEarlyFailure(t *testing.T) {
	t.Setenv(setupTokenEnvironment, "never-print-or-persist-this-token")
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
	command.SetErr(&output)

	err := installCmd.RunE(command, nil)

	if err == nil || strings.Contains(output.String(), "never-print-or-persist-this-token") || strings.Contains(err.Error(), "never-print-or-persist-this-token") {
		t.Fatal("early failure exposed the provided token")
	}
	if _, exists := os.LookupEnv(setupTokenEnvironment); exists {
		t.Fatal("early failure retained the provided token environment")
	}
}

func TestInstallUsesProvidedSetupTokenWithoutTerminalPrompt(t *testing.T) {
	const token = "ark_tk_isolated_setup_fixture"
	t.Setenv(setupTokenEnvironment, token)
	registered := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"registration_protocol":"client-credential-v1"}`)
			return
		}
		registered = true
		if r.URL.Path != "/api/v1/agent/register" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("provided setup token did not reach enrollment")
		}
		if _, exists := os.LookupEnv(setupTokenEnvironment); exists {
			t.Error("enrollment retained the setup token in the process environment")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	previousPath, previousURL, previousServiceOnly := configPath, installAPIURL, installServiceOnly
	configPath = filepath.Join(t.TempDir(), "config.yaml")
	installAPIURL, installServiceOnly = server.URL, false
	t.Cleanup(func() {
		configPath, installAPIURL, installServiceOnly = previousPath, previousURL, previousServiceOnly
	})
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(&output)
	command.SetErr(&output)

	err := installCmd.RunE(command, nil)

	if !registered || !errors.Is(err, enrollment.ErrInvalidToken) {
		t.Fatalf("provided token did not bypass the terminal prompt: %v", err)
	}
	if strings.Contains(output.String(), token) || strings.Contains(err.Error(), token) {
		t.Fatal("provided setup token appeared in setup output")
	}
	entries, err := os.ReadDir(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		contents, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), entry.Name()))
		if err != nil || strings.Contains(string(contents), token) {
			t.Fatal("provided setup token was persisted during enrollment")
		}
	}
}

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
	for _, want := range []string{"FiveManage database backups", "[1/5] Connect to FiveManage", "x Couldn't connect to FiveManage."} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing setup feedback %q", want)
		}
	}
	for _, unwanted := range []string{"[2/5]", "[3/5]", "[4/5]", "[5/5]", "all set", "+ ", "\x1b"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("early failure emitted %q", unwanted)
		}
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("early setup failure wrote a config")
	}
}
