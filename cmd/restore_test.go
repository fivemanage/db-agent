package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/restore"
	"github.com/fivemanage/db-agent/internal/securefile"
	"gopkg.in/yaml.v3"
)

func restoreFixture(t *testing.T) (string, string, key.Key, []byte, artifact.Metadata) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	keyPath := filepath.Join(dir, "encryption.key")
	k, _, err := key.Ensure(keyPath, true)
	if err != nil {
		t.Fatal(err)
	}
	settings := config.Config{
		AgentCredential: "dac_" + strings.Repeat("a", 43), APIURL: "https://api.example.com",
		Database:   config.DatabaseConfig{Password: "private-db-password"},
		Encryption: config.EncryptionConfig{KeyFile: "encryption.key", KeyID: k.ID(), KeyAcknowledged: true},
	}
	contents, err := yaml.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := securefile.Create(path, contents); err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	metadata, err := artifact.Encrypt(context.Background(), &encrypted, strings.NewReader("CREATE DATABASE original;\nUSE original;\nSELECT 'private SQL';\n"), k, artifact.Zstd)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "backup.dbak")
	if err := os.WriteFile(input, encrypted.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return path, input, k, encrypted.Bytes(), metadata
}

func TestRestoreCommandUsesLocalKeyAutomaticallyWithoutDatabaseReadiness(t *testing.T) {
	path, input, k, _, _ := restoreFixture(t)
	previous := configPath
	configPath = path
	t.Cleanup(func() { configPath = previous })
	outputPath := filepath.Join(filepath.Dir(path), "restore.sql")
	var output bytes.Buffer
	command := newRestoreCommand()
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetIn(strings.NewReader(""))
	command.SetArgs([]string{"--file", input, "--output", outputPath})

	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}

	contents, err := securefile.Read(outputPath, 4096)
	if err != nil || !strings.Contains(string(contents), "USE original;") {
		t.Fatalf("automatic key restore failed: %v", err)
	}
	text := output.String()
	if !strings.Contains(text, "No database was changed") || !strings.Contains(text, "ORIGINAL database names") || strings.Contains(text, k.RecoveryString()) || strings.Contains(text, "private-db-password") || strings.Contains(text, "private SQL") {
		t.Fatalf("restore output lost guidance or exposed secrets: %s", text)
	}
	after, err := key.Load(filepath.Join(filepath.Dir(path), "encryption.key"))
	if err != nil || after.Bytes() != k.Bytes() {
		t.Fatal("restore rotated or changed the local key")
	}
}

func TestOfflineRecoveryNeedsNoConfigOrAgentCredential(t *testing.T) {
	path, input, _, _, _ := restoreFixture(t)
	keyPath := filepath.Join(filepath.Dir(path), "encryption.key")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(filepath.Dir(path), "recovered.sql")
	var output bytes.Buffer

	err := runConfiguredRestore(context.Background(), path, restore.Request{File: input, Output: outputPath}, keyPath, restore.Options{}, &output)

	if err != nil {
		t.Fatalf("offline recovery required the old agent: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("offline recovery created or changed config")
	}
}

func TestRemoteRestoreSeparatesAgentAuthenticationFromStorage(t *testing.T) {
	path, _, k, encrypted, metadata := restoreFixture(t)
	var downloads, lookups atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("local authentication reached storage")
		}
		w.Write(encrypted)
	}))
	defer storage.Close()
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/backups/backup_1/download" || r.Header.Get("Authorization") != "Bearer dac_"+strings.Repeat("a", 43) || r.ContentLength > 0 || strings.Contains(r.URL.String(), k.RecoveryString()) {
			t.Error("download lookup exposed secrets or changed protocol")
		}
		json.NewEncoder(w).Encode(controlplane.DownloadBackupResponse{DownloadURL: storage.URL + "/backup?signature=private", ExpiresAt: time.Now().Add(time.Hour), SizeBytes: metadata.SizeBytes, Checksum: metadata.Checksum})
	}))
	defer plane.Close()
	settings, err := config.LoadDiagnostic(path)
	if err != nil {
		t.Fatal(err)
	}
	settings.APIURL = plane.URL
	contents, err := yaml.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(filepath.Dir(path), "remote.sql")
	var output bytes.Buffer

	err = runConfiguredRestore(context.Background(), path, restore.Request{BackupID: "backup_1", Output: outputPath}, "", restore.Options{Transport: storage.Client().Transport}, &output)

	if err != nil || downloads.Load() != 1 || lookups.Load() != 1 {
		t.Fatalf("remote restore failed: %v", err)
	}
	if strings.Contains(output.String(), k.RecoveryString()) || strings.Contains(output.String(), "signature=") {
		t.Fatal("remote restore output leaked secrets")
	}
}

func TestMissingOrMismatchedLocalKeyIsNeverRegenerated(t *testing.T) {
	for _, failure := range []string{"missing", "mismatched", "unacknowledged"} {
		t.Run(failure, func(t *testing.T) {
			path, input, _, _, _ := restoreFixture(t)
			settings, err := config.LoadDiagnostic(path)
			if err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(filepath.Dir(path), "encryption.key")
			if failure == "missing" {
				if err := os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			} else {
				if failure == "mismatched" {
					settings.Encryption.KeyID = strings.Repeat("f", 64)
				} else {
					settings.Encryption.KeyAcknowledged = false
				}
				contents, err := yaml.Marshal(settings)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, contents, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(keyPath)
			outputPath := filepath.Join(filepath.Dir(path), "failed.sql")
			var output bytes.Buffer

			err = runConfiguredRestore(context.Background(), path, restore.Request{File: input, Output: outputPath}, "", restore.Options{}, &output)

			if err == nil || output.Len() != 0 {
				t.Fatal("unusable local key was accepted or prompted")
			}
			after, _ := os.ReadFile(keyPath)
			if !bytes.Equal(before, after) {
				t.Fatal("restore generated or changed the key")
			}
			if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unusable local key produced output")
			}
		})
	}
}

func TestRestoreCommandRejectsAmbiguousOrUnsafeFlags(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--backup-id", "backup_1", "--file", "backup.dbak"},
		{"--backup-id", "backup_1", "--key-file", "key"},
		{"--file", "backup.dbak", "--timeout", "0s"},
		{"--file", "backup.dbak", "--max-output-bytes", "-1"},
		{"--file", "backup.dbak", "--key", "never-accept-key-on-command-line"},
	} {
		command := newRestoreCommand()
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		if err := command.Execute(); err == nil {
			t.Fatalf("unsafe restore flags accepted: %v", args)
		}
	}
}

func TestRestoreImportGuidanceDoesNotInjectShellCommands(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		var output bytes.Buffer
		printRestoreImport(&output, "restore\nmalicious-command.sql", goos)
		if strings.Contains(output.String(), "malicious-command") || strings.Contains(output.String(), "mysql --host=") {
			t.Fatal("control characters reached import command")
		}
	}
	var output bytes.Buffer
	printRestoreImport(&output, "restore'$(malicious).sql", "linux")
	if !strings.Contains(output.String(), "'\"'\"'") {
		t.Fatal("POSIX filename was not shell-quoted")
	}
	output.Reset()
	printRestoreImport(&output, "restore%PRIVATE%.sql", "windows")
	if strings.Contains(output.String(), "mysql --host=") {
		t.Fatal("Windows environment expansion reached import command")
	}
}
