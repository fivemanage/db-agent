package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
)

func TestRegistrationRetriesKeepCandidateAndMetadata(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.Header.Get("Authorization") != "" {
				t.Error("discovery included credentials")
			}
			io.WriteString(w, `{"registration_protocol":"client-credential-v1"}`)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Authorization") != "Bearer ark_tk_once" || body["agent_credential"] != testCredential || body["hostname"] != "local-host" || body["db_driver"] != "mariadb" || body["db_version"] != "11.4.0" || len(body) != 7 {
			t.Error("registration protocol fields changed or included local secrets")
		}
		if posts.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"agent_credential": testCredential})
	}))
	defer server.Close()
	c, err := NewEnrollment(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = time.Millisecond
	result, err := c.Register(context.Background(), "ark_tk_once", testCredential, RegistrationMetadata{AgentVersion: "dev", Hostname: "local-host", OS: "linux", Arch: "arm64", DatabaseMetadata: DatabaseMetadata{Driver: "mariadb", Version: "11.4.0"}})
	if err != nil || result.AgentCredential != testCredential || posts.Load() != 2 {
		t.Fatalf("safe registration replay failed: %v", err)
	}
}

func TestHeartbeatValidatesAndDeliversAllowedOverrides(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"config_overrides":{"backup":{"schedule":"0 5 * * *"},"auto_update":false}}`)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	called := false
	c.OnOverrides = func(_ context.Context, o config.DashboardOverrides) error {
		called = true
		if o.AutoUpdate == nil || *o.AutoUpdate || o.Backup.Schedule == nil || *o.Backup.Schedule != "0 5 * * *" {
			t.Error("override contents changed")
		}
		return nil
	}
	if err := c.Heartbeat(context.Background(), State{Status: Idle}); err != nil || !called {
		t.Fatalf("override not delivered: %v", err)
	}
}

func TestBackupContractsUseAPIAuthAndStableInitiationIdentity(t *testing.T) {
	var attempts atomic.Int32
	expires := time.Now().Add(time.Hour)
	const key = "stable-request-identity-0001"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testCredential {
			t.Error("issued credential missing")
		}
		switch r.URL.Path {
		case "/v1/backups/initiate":
			if r.Header.Get("Idempotency-Key") != key {
				t.Error("initiation identity changed")
			}
			var body map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 1 || body["started_at"] == nil {
				t.Error("initiation included unexpected data")
			}
			if attempts.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(InitiateBackupResponse{BackupID: "backup_1", UploadURL: "https://storage.example/object?signature=private", ExpiresAt: expires})
		case "/v1/backups/backup_1/complete":
			var body map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 4 || body["checksum"] == nil {
				t.Error("completion contract invalid")
			}
			io.WriteString(w, `{}`)
		case "/v1/backups/backup_1/download":
			if r.Method != http.MethodGet {
				t.Error("download lookup must be GET")
			}
			json.NewEncoder(w).Encode(DownloadBackupResponse{DownloadURL: "https://storage.example/object?signature=private", ExpiresAt: expires})
		default:
			t.Error("unexpected control-plane path")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	started, err := c.InitiateBackup(context.Background(), InitiateBackupRequest{RequestID: key, StartedAt: time.Now()})
	if err != nil || started.BackupID != "backup_1" || attempts.Load() != 2 {
		t.Fatalf("initiation failed: %v", err)
	}
	if err := c.CompleteBackup(context.Background(), started.BackupID, CompleteBackupRequest{Status: "success", SizeBytes: 12, DurationSeconds: 2.5, Checksum: "sha256:" + strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DownloadBackup(context.Background(), started.BackupID); err != nil {
		t.Fatal(err)
	}
}

func TestUnsafeBackupRequestsNeverReachNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	c := testClient(t, server.URL)
	if _, err := c.InitiateBackup(context.Background(), InitiateBackupRequest{StartedAt: time.Now()}); err == nil {
		t.Fatal("missing idempotency key accepted")
	}
	if _, err := c.DownloadBackup(context.Background(), "../private"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if err := c.CompleteBackup(context.Background(), "backup", CompleteBackupRequest{Status: "success"}); err == nil {
		t.Fatal("success without checksum accepted")
	}
	if err := c.CompleteBackup(context.Background(), "backup", CompleteBackupRequest{Status: "failed", Problem: "private-password"}); err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatal("raw backup error accepted or echoed")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid backup metadata reached network")
	}
}

func TestUnsafeTransferURLsAreRejectedWithoutEchoingSignatures(t *testing.T) {
	for _, target := range []string{"http://storage.example/private-signature", "https://user:private-signature@storage.example/object", "https://storage.example/object#private-signature"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(DownloadBackupResponse{DownloadURL: target, ExpiresAt: time.Now().Add(time.Hour)})
		}))
		_, err := testClient(t, server.URL).DownloadBackup(context.Background(), "backup")
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private-signature") {
			t.Fatal("unsafe transfer URL accepted or exposed")
		}
	}
}

func TestUnapprovedHeartbeatOverridesNeverReachConsumer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"config_overrides":{"encryption":{"key_file":"private-value"}}}`)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	called := false
	c.OnOverrides = func(context.Context, config.DashboardOverrides) error { called = true; return nil }
	err := c.Heartbeat(context.Background(), State{Status: Idle})
	if err == nil || called || strings.Contains(err.Error(), "private-value") {
		t.Fatal("secret override reached consumer or leaked")
	}
}
