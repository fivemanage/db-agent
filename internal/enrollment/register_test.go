package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
)

func TestRegisterExchangesSetupToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"registration_protocol":"client-credential-v1"}`)
			return
		}
		if r.URL.Path != "/v1/agent/register" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ark_tk_setup" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			controlplane.RegistrationMetadata
			Credential string `json:"agent_credential"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.AgentVersion != "0.1.0" || body.Hostname == "" || body.OS == "" || body.Arch == "" {
			t.Errorf("registration metadata = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"agent_credential": body.Credential})
	}))
	defer server.Close()

	credential, err := Register(context.Background(), server.URL, "ark_tk_setup", "0.1.0")
	if err != nil || !strings.HasPrefix(credential, "dac_") || len(credential) != 47 {
		t.Fatalf("Register() failed: %v", err)
	}
}

func TestRegisterRejectsInvalidTokenWithoutLeakingIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"registration_protocol":"client-credential-v1"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"ark_tk_secret"}`))
	}))
	defer server.Close()

	_, err := Register(context.Background(), server.URL, "ark_tk_secret", "dev")
	if !errors.Is(err, ErrInvalidToken) || strings.Contains(err.Error(), "ark_tk_secret") {
		t.Fatalf("Register() error = %v", err)
	}
}

func TestRegisterDoesNotFollowRedirectWithToken(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	_, err := Register(context.Background(), source.URL, "ark_tk_secret", "dev")
	if err == nil || redirected {
		t.Fatalf("Register() error = %v, redirected = %v", err, redirected)
	}
}

func TestRegisterRejectsUnencryptedRemoteURL(t *testing.T) {
	_, err := Register(context.Background(), "http://example.com", "ark_tk_secret", "dev")
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("Register() error = %v", err)
	}
}

func TestRegisterRequiresCredentialInResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"registration_protocol":"client-credential-v1"}`)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	_, err := Register(context.Background(), server.URL, "ark_tk_secret", "dev")
	if err == nil || !strings.Contains(err.Error(), "pending credential") {
		t.Fatalf("Register() error = %v", err)
	}
}

func TestEnrollRecoversLostResponseAcrossRestartsWithoutSavingToken(t *testing.T) {
	var mu sync.Mutex
	var candidate string
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/agent/heartbeat" {
			if r.Header.Get("Authorization") == "Bearer "+candidate {
				io.WriteString(w, `{}`)
			} else {
				w.WriteHeader(401)
			}
			return
		}
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"registration_protocol":"client-credential-v1"}`)
			return
		}
		var body struct {
			Credential string `json:"agent_credential"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if candidate != "" && candidate != body.Credential {
			t.Error("enrollment rotated its candidate")
		}
		candidate = body.Credential
		posts++
		// The server committed, but every response in this process was lost.
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	_, err := Enroll(context.Background(), path, server.URL, "ark_tk_private-one-time", "dev")
	if err == nil {
		t.Fatal("lost responses unexpectedly succeeded")
	}
	pending, err := config.LoadPendingEnrollment(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path + ".enrollment-pending")
	if err != nil || strings.Contains(string(contents), "ark_tk_private-one-time") {
		t.Fatal("enrollment token was persisted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfirmed config written")
	}
	recovered, err := Recover(context.Background(), path, server.URL, "dev")
	if err != nil || !recovered {
		t.Fatalf("recovery failed: %v", err)
	}
	saved, err := config.LoadAgent(path)
	if err != nil || saved.AgentCredential != pending.Credential {
		t.Fatal("recovery rotated or lost the issued credential")
	}
	mu.Lock()
	count := posts
	mu.Unlock()
	if count != 3 {
		t.Fatal("registration retries were not bounded")
	}
	if _, err := os.Stat(path + ".enrollment-pending"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed pending state not cleaned up")
	}
}

func TestOldServerCannotConsumeTokenDuringCapabilityNegotiation(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.WriteHeader(405)
	}))
	defer server.Close()
	_, err := Register(context.Background(), server.URL, "ark_tk_private", "dev")
	if err == nil || posts != 0 {
		t.Fatal("token sent to a server without replay support")
	}
}
