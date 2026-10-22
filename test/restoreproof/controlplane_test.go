//go:build integration

package restoreproof

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/restore"
	"github.com/fivemanage/db-agent/internal/restorejob"
	"gopkg.in/yaml.v3"
)

// Keep the SQL containers network-isolated. This test-only loopback proxy
// carries HTTP through the parent test's stdin/stdout to the actual TS handlers.
func TestControlPlaneRestoreBridge(t *testing.T) {
	if os.Getenv("DB_AGENT_RESTORE_BRIDGE") != "1" {
		t.Skip("run DB_AGENT_RESTORE_COMBINED=1 bash scripts/test-agent-restores.sh in the control-plane repository")
	}
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.exec(f.admin, "CREATE VIEW "+qi(f.source)+".controlplane_balances AS SELECT id,credits FROM "+qi(f.source)+".players")
	x := newExecutionFixture(f, f.nativeDump())
	credential := sha256.Sum256([]byte(f.user))
	x.settings.AgentCredential = "dac_" + base64.RawURLEncoding.EncodeToString(credential[:])
	x.job.DatabaseID, x.job.AgentID, x.job.BackupID = f.user+"_database", f.user+"_agent", f.user+"_backup"
	f.target = x.job.Destination()
	registerExecutionCleanup(x)
	before, grants := f.snapshotSchema(false), backupGrantHash(f, x.settings.Database)
	var mu sync.Mutex
	decoder := json.NewDecoder(os.Stdin)
	bridge := func(method, path string, body json.RawMessage) (int, json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		message, err := json.Marshal(struct {
			Method     string          `json:"method"`
			Path       string          `json:"path"`
			Credential string          `json:"credential"`
			Body       json.RawMessage `json:"body"`
		}{method, path, x.settings.AgentCredential, body})
		if err != nil {
			t.Error("cannot encode bridge request")
			return 500, nil
		}
		if _, err := os.Stdout.Write(append(append([]byte("RESTORE_BRIDGE "), message...), '\n')); err != nil {
			t.Error("cannot send bridge request")
			return 500, nil
		}
		var response struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		}
		if decoder.Decode(&response) != nil {
			t.Error("invalid control-plane bridge response")
			return 500, nil
		}
		return response.Status, response.Body
	}
	metadata, _ := json.Marshal(struct {
		Job         restorejob.Job `json:"job"`
		DownloadURL string         `json:"downloadUrl"`
		Destination string         `json:"destination"`
	}{x.job, x.storage.URL + "/artifact?signature=public_fixture", x.job.Destination()})
	if status, _ := bridge("POST", "/fixture", metadata); status != 200 {
		t.Fatal("control-plane fixture setup failed")
	}
	terminal := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		if len(body) == 0 {
			body = []byte("null")
		}
		status, response := bridge(r.Method, r.URL.Path, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(response)
		var report controlplane.RestoreReport
		if status == 200 && json.Unmarshal(body, &report) == nil && report.Phase == "succeeded" {
			select {
			case terminal <- struct{}{}:
			default:
			}
		}
	}))
	defer proxy.Close()
	x.settings.APIURL = proxy.URL
	data, err := yaml.Marshal(x.settings)
	if err != nil || os.WriteFile(x.path, data, 0600) != nil {
		t.Fatal("cannot save bridge configuration")
	}
	client, err := controlplane.New(proxy.URL, x.settings.AgentCredential, "0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := restorejob.NewWorker(x.path, client, restorejob.Options{Download: restore.Options{Transport: x.storage.Client().Transport, ReserveBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-terminal:
	case err := <-done:
		t.Fatalf("worker stopped before confirmed success: %v", err)
	case <-ctx.Done():
		t.Fatal("control-plane restore did not complete")
	}
	observer := x.runner(restorejob.Options{})
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := observer.Snapshot(x.job)
		if err == nil && !state.Pending() {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("terminal acknowledgement was not saved")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.scalar(f.admin, "SELECT credits FROM "+qi(x.job.Destination())+".controlplane_balances WHERE id=1") != 100 || f.snapshotSchema(false) != before || backupGrantHash(f, x.settings.Database) != grants {
		t.Fatal("backend-delivered restore is unusable or changed source/backup grants")
	}
	assertExecutionAccountLocked(t, x)
	f.exec(f.admin, "UPDATE "+qi(x.job.Destination())+".players SET credits=777 WHERE id=1")
	state, err := observer.Run(f.ctx, x.job)
	if err != nil || state.Phase != restorejob.Succeeded || x.gets.Load() != 1 || f.scalar(f.admin, "SELECT credits FROM "+qi(x.job.Destination())+".controlplane_balances WHERE id=1") != 777 {
		t.Fatal("duplicate backend job repeated import")
	}
}
