package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterExchangesSetupToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/register" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ark_tk_setup" {
			t.Errorf("Authorization = %q", got)
		}
		var body registrationRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.AgentVersion != "0.1.0" || body.Hostname == "" || body.OS == "" || body.Arch == "" {
			t.Errorf("registration metadata = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"agent_credential":"dac_issued"}`))
	}))
	defer server.Close()

	credential, err := Register(context.Background(), server.URL, "ark_tk_setup", "0.1.0")
	if err != nil || credential != "dac_issued" {
		t.Fatalf("Register() = %q, %v", credential, err)
	}
}

func TestRegisterRejectsInvalidTokenWithoutLeakingIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	_, err := Register(context.Background(), server.URL, "ark_tk_secret", "dev")
	if err == nil || !strings.Contains(err.Error(), "agent credential") {
		t.Fatalf("Register() error = %v", err)
	}
}
