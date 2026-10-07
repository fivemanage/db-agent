package diagnostic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/key"
)

func TestAPIReachabilityDoesNotSendSecrets(t *testing.T) {
	for _, status := range []int{200, 401, 404, 503, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.ContentLength > 0 || r.URL.RawQuery != "" {
					t.Error("probe sent credentials or payload")
				}
				w.WriteHeader(status)
				w.Write([]byte("never-print-this"))
			}))
			defer server.Close()
			check := CheckAPI(context.Background(), server.URL)
			want := status < 300 || status >= 400 && status < 500
			if check.Passed != want || strings.Contains(check.Message, "never-print-this") {
				t.Fatalf("check=%+v", check)
			}
		})
	}
}

func TestAPIRejectsUnsafeURLsAndRedactsErrors(t *testing.T) {
	for _, value := range []string{"https://user:never-print-this@example.com", "http://example.com", "https://example.com/?key=never-print-this"} {
		check := CheckAPI(context.Background(), value)
		if check.Passed || strings.Contains(check.Message, "never-print-this") {
			t.Fatalf("check=%+v", check)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	if CheckAPI(context.Background(), server.URL).Passed {
		t.Fatal("unreachable API passed")
	}
}

func TestKeyAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encryption.key")
	if CheckKey(path).Passed || CheckKey("").Passed {
		t.Fatal("missing key passed")
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if CheckKey(path).Passed {
		t.Fatal("empty key passed")
	}
	if err := os.WriteFile(path, []byte("never-print-this"), 0600); err != nil {
		t.Fatal(err)
	}
	check := CheckKey(path)
	if check.Passed || strings.Contains(check.Message, "never-print-this") {
		t.Fatalf("check=%+v", check)
	}
	valid := filepath.Join(t.TempDir(), "encryption.key")
	k, _, err := key.Ensure(valid, true)
	if err != nil {
		t.Fatal(err)
	}
	if check := CheckKey(valid); !check.Passed || strings.Contains(check.Message, k.RecoveryString()) {
		t.Fatal("valid private key rejected or disclosed")
	}
	if CheckKey(filepath.Dir(path)).Passed {
		t.Fatal("directory key passed")
	}
}

func TestRegistrationConfigDoesNotPassDatabaseCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	checks := Run(context.Background(), config.Config{AgentCredential: "dac_never-print-this", APIURL: server.URL})
	apiPassed, dbFailed := false, false
	for _, check := range checks {
		if strings.Contains(check.Message, "never-print-this") {
			t.Fatal("credential leaked")
		}
		if check.Name == "Control-plane reachability" {
			apiPassed = check.Passed
		}
		if check.Name == "Database connectivity" {
			dbFailed = !check.Passed
		}
	}
	if !apiPassed || !dbFailed {
		t.Fatalf("checks=%+v", checks)
	}
}
