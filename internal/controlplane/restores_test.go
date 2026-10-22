package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRestoreDeliveryIsCredentialAndArtifactBound(t *testing.T) {
	for _, mismatch := range []string{"", "agent", "database", "size", "checksum", "active"} {
		t.Run(mismatch, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer dac_"+strings.Repeat("A", 43) || r.URL.Path != "/api/v1/agent/restores/poll" {
					t.Error("wrong authenticated restore transport")
				}
				job := RestoreJob{ID: "job", DatabaseID: "database", AgentID: "agent", BackupID: "backup", SizeBytes: 100, Checksum: "sha256:" + strings.Repeat("a", 64)}
				switch mismatch {
				case "agent":
					job.AgentID = "other"
				case "database":
					job.DatabaseID = "other"
				case "size":
					job.SizeBytes = 1
				case "checksum":
					job.Checksum = "raw-secret"
				case "active":
					job.ID = "other"
				}
				json.NewEncoder(w).Encode(RestorePollResponse{Protocol: 1, DatabaseID: "database", AgentID: "agent", Job: &job})
			}))
			defer server.Close()
			client, err := New(server.URL, "dac_"+strings.Repeat("A", 43), "0.1.0", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.PollRestores(context.Background(), RestorePollRequest{Protocol: 1, Readiness: "ready", ActiveJobID: "job"})
			if (err == nil) != (mismatch == "") {
				t.Fatalf("unsafe restore response accepted: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "raw-secret") {
				t.Fatal("untrusted response entered an error")
			}
		})
	}
}

func TestRestoreReportsRequireClassifiedProblemsAndExactRevisionAck(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(RestoreReportResponse{Revision: 1})
	}))
	defer server.Close()
	client, err := New(server.URL, "dac_"+strings.Repeat("A", 43), "0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReportRestore(context.Background(), "job", RestoreReport{Revision: 1, Phase: "failed", Problem: "private-password"}); err == nil || calls != 0 {
		t.Fatal("raw report problem sent")
	}
	if _, err := client.ReportRestore(context.Background(), "../job", RestoreReport{Revision: 1, Phase: "accepted"}); err == nil || calls != 0 {
		t.Fatal("unsafe report ID sent")
	}
	if _, err := client.ReportRestore(context.Background(), "job", RestoreReport{Revision: 1, Phase: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReportRestore(context.Background(), "job", RestoreReport{Revision: 2, Phase: "importing", WritesStarted: true}); err == nil {
		t.Fatal("stale acknowledgement accepted")
	}
	_, err = client.PollRestores(context.Background(), RestorePollRequest{Protocol: 1, Readiness: "private-password"})
	if err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatal("readiness was not classified")
	}
	if errors.Is(err, ErrCredentialRejected) {
		t.Fatal("local validation was confused with remote access rejection")
	}
}
