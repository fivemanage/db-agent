package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupPollIsAuthenticatedAndAcceptsOnlyFinishRequests(t *testing.T) {
	keyFile := filepath.Join(os.TempDir(), "encryption.key")
	for _, kind := range []string{SetupFinish, "inspect"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer dac_"+strings.Repeat("A", 43) || r.URL.Path != "/api/v1/agent/setup/poll" {
					t.Error("wrong authenticated setup transport")
				}
				var status SetupStatus
				if json.NewDecoder(r.Body).Decode(&status) != nil || status.KeyFile != keyFile {
					t.Error("setup status not sent")
				}
				json.NewEncoder(w).Encode(SetupPollResponse{Protocol: 1, Request: &SetupRequest{ID: "request", Kind: kind}})
			}))
			defer server.Close()
			client, err := New(server.URL, "dac_"+strings.Repeat("A", 43), "0.1.0", nil)
			if err != nil {
				t.Fatal(err)
			}

			response, err := client.PollSetup(context.Background(), SetupStatus{Protocol: 1, KeyFile: keyFile})

			if (err == nil) != (kind == SetupFinish) {
				t.Fatalf("unexpected setup request handling: %v", err)
			}
			if err == nil && response.Request.ID != "request" {
				t.Fatal("finish request was not returned")
			}
		})
	}
}

func TestSetupStatusRejectsInconsistentCompletion(t *testing.T) {
	client := newClient("http://127.0.0.1:1", "dac_"+strings.Repeat("A", 43), "0.1.0", nil)

	_, err := client.PollSetup(context.Background(), SetupStatus{Protocol: 1, Complete: true})

	if err == nil {
		t.Fatal("completion without a database and acknowledged key was sent")
	}
}
