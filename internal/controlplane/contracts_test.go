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
		case "/api/v1/backups/initiate":
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
		case "/api/v1/backups/backup_1/complete":
			var body map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 4 || body["checksum"] == nil {
				t.Error("completion contract invalid")
			}
			io.WriteString(w, `{}`)
		case "/api/v1/backups/backup_1/download":
			if r.Method != http.MethodGet {
				t.Error("download lookup must be GET")
			}
			json.NewEncoder(w).Encode(DownloadBackupResponse{DownloadURL: "https://storage.example/object?signature=private", ExpiresAt: expires, SizeBytes: 100, Checksum: "sha256:" + strings.Repeat("a", 64)})
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

func TestMultipartBackupContracts(t *testing.T) {
	key, err := NewRequestID()
	if err != nil {
		t.Fatal(err)
	}

	expires := time.Now().Add(time.Hour).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/backups/initiate":
			var in InitiateBackupRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.UploadMode != "multipart" || in.PartSizeBytes != 64<<20 {
				t.Errorf("unexpected multipart initiate payload: %+v (%v)", in, err)
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(InitiateBackupResponse{
				BackupID:      "backup_mp",
				UploadMode:    "multipart",
				PartSizeBytes: 64 << 20,
				MaxSizeBytes:  16 << 30,
				ExpiresAt:     expires,
				Parts: []MultipartPartURL{
					{PartNumber: 1, UploadURL: "https://storage.example/part-1?sig=secret"},
					{PartNumber: 2, UploadURL: "https://storage.example/part-2?sig=secret"},
				},
			})
		case "/api/v1/backups/backup_mp/parts":
			var body struct {
				PartNumbers []int `json:"part_numbers"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.PartNumbers) != 2 || body.PartNumbers[0] != 3 || body.PartNumbers[1] != 4 {
				t.Errorf("unexpected part sign request: %+v (%v)", body, err)
			}
			json.NewEncoder(w).Encode(SignBackupPartsResponse{
				ExpiresAt: expires,
				Parts: []MultipartPartURL{
					{PartNumber: 3, UploadURL: "https://storage.example/part-3?sig=secret"},
					{PartNumber: 4, UploadURL: "https://storage.example/part-4?sig=secret"},
				},
			})
		case "/api/v1/backups/backup_mp/complete":
			var in CompleteBackupRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Parts) != 2 || in.Parts[0].PartNumber != 1 || in.Parts[1].ETag != "etag2" {
				t.Errorf("unexpected multipart complete payload: %+v (%v)", in, err)
			}
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	c := testClient(t, server.URL)

	started, err := c.InitiateBackup(context.Background(), InitiateBackupRequest{
		RequestID:     key,
		StartedAt:     time.Now().UTC(),
		UploadMode:    "multipart",
		PartSizeBytes: 64 << 20,
	})
	if err != nil || started.BackupID != "backup_mp" || started.UploadMode != "multipart" || len(started.Parts) != 2 {
		t.Fatalf("multipart initiation failed: %+v %v", started, err)
	}

	signed, err := c.SignBackupParts(context.Background(), started.BackupID, []int{3, 4})
	if err != nil || len(signed.Parts) != 2 || signed.Parts[0].PartNumber != 3 {
		t.Fatalf("part signing failed: %+v %v", signed, err)
	}

	if err := c.CompleteBackup(context.Background(), started.BackupID, CompleteBackupRequest{
		Status:          "success",
		SizeBytes:       100,
		DurationSeconds: 1.5,
		Checksum:        "sha256:" + strings.Repeat("a", 64),
		Parts: []CompletedPart{
			{PartNumber: 1, ETag: "etag1"},
			{PartNumber: 2, ETag: "etag2"},
		},
	}); err != nil {
		t.Fatalf("multipart completion failed: %v", err)
	}

	if _, err := c.SignBackupParts(context.Background(), started.BackupID, []int{1, 1}); err == nil {
		t.Fatal("duplicate part numbers accepted")
	}
	if err := c.CompleteBackup(context.Background(), started.BackupID, CompleteBackupRequest{
		Status:          "success",
		SizeBytes:       100,
		DurationSeconds: 1,
		Checksum:        "sha256:" + strings.Repeat("a", 64),
		Parts:           []CompletedPart{{PartNumber: 2, ETag: "etag2"}},
	}); err == nil {
		t.Fatal("non-contiguous completed parts accepted")
	}
}

func TestUnsafeTransferURLsAreRejectedWithoutEchoingSignatures(t *testing.T) {
	for _, target := range []string{"http://storage.example/private-signature", "https://user:private-signature@storage.example/object", "https://storage.example/object#private-signature"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(DownloadBackupResponse{DownloadURL: target, ExpiresAt: time.Now().Add(time.Hour), SizeBytes: 100, Checksum: "sha256:" + strings.Repeat("a", 64)})
		}))
		_, err := testClient(t, server.URL).DownloadBackup(context.Background(), "backup")
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private-signature") {
			t.Fatal("unsafe transfer URL accepted or exposed")
		}
	}
}

func TestDownloadRequiresTrustedIntegrityMetadata(t *testing.T) {
	for _, metadata := range []string{
		``, `,"size_bytes":100`, `,"checksum":"sha256:` + strings.Repeat("a", 64) + `"`,
		`,"size_bytes":-1,"checksum":"sha256:` + strings.Repeat("a", 64) + `"`,
		`,"size_bytes":100,"checksum":"private-checksum"`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"download_url":"https://storage.example/private-signature","expires_at":"2099-01-01T00:00:00Z"`+metadata+`}`)
		}))
		_, err := testClient(t, server.URL).DownloadBackup(context.Background(), "backup_1")
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe download metadata accepted or exposed: %v", err)
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
