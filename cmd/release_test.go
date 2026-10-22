package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/fivemanage/db-agent/internal/enrollment"
	"github.com/spf13/cobra"
)

func TestBuildVersionMatchesRegistration(t *testing.T) {
	reports := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"registration_protocol": "client-credential-v1"})
			return
		}
		var body struct {
			Version    string `json:"agent_version"`
			Credential string `json:"agent_credential"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		reports <- body.Version
		json.NewEncoder(w).Encode(map[string]string{"agent_credential": body.Credential})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	_, err := ensureEnrollment(context.Background(), path, server.URL, func() (string, error) {
		return "release-test-token", nil
	}, func(ctx context.Context, apiURL, token, buildVersion string) (string, error) {
		return enrollment.Enroll(ctx, path, apiURL, token, buildVersion)
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	if err := versionCmd.RunE(command, nil); err != nil {
		t.Fatal(err)
	}
	reported := <-reports
	if reported != version || output.String() != reported+"\n" || rootCmd.Version != reported {
		t.Fatalf("registration %q differs from CLI %q or Cobra %q", reported, output.String(), rootCmd.Version)
	}
}

func TestVersionJSONIncludesOnlyPublicBuildSettings(t *testing.T) {
	oldJSON, oldKey, oldAPI := versionJSON, updatePublicKey, defaultAPIURL
	versionJSON, updatePublicKey, defaultAPIURL = true, "public-key", "https://api.example.com"
	t.Cleanup(func() { versionJSON, updatePublicKey, defaultAPIURL = oldJSON, oldKey, oldAPI })
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	if err := versionCmd.RunE(command, nil); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"version": version, "public_key": updatePublicKey, "api_url": defaultAPIURL, "go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}
	if len(got) != len(want) {
		t.Fatalf("unexpected metadata fields: %v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q; want %q", key, got[key], value)
		}
	}
}
