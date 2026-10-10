package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testCredential = "dac_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func testClient(t *testing.T, apiURL string) *Client {
	t.Helper()
	c, err := New(apiURL, testCredential, "0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = time.Millisecond
	return c
}

func TestHeartbeatWireContract(t *testing.T) {
	states := []State{{Status: Idle}, {Status: BackingUp}, {Status: Uploading}, {Status: Error, Problem: SetupIncomplete}}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/api/v1/agent/heartbeat" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testCredential || r.Header.Get("Content-Type") != "application/json" {
			t.Error("heartbeat did not use the authenticated protocol")
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		index := int(calls.Add(1)) - 1
		if index >= len(states) {
			t.Error("unexpected extra request")
			return
		}
		if payload["agent_version"] != "0.1.0" || payload["status"] != string(states[index].Status) {
			t.Error("unexpected version or status")
		}
		if states[index].Status == Error {
			if len(payload) != 3 || !strings.Contains(payload["last_error"], "prerequisites") {
				t.Error("missing classified setup problem")
			}
		} else if len(payload) != 2 {
			t.Error("heartbeat contains unexpected local data or old errors")
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	c := testClient(t, server.URL+"/base/")
	for _, state := range states {
		if err := c.Heartbeat(context.Background(), state); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != int32(len(states)) {
		t.Fatal("heartbeat requests missing")
	}
}

func TestHeartbeatRetriesOnlyTransientHTTPFailures(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 413, 422, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(code)
					io.WriteString(w, "private-database-password "+testCredential)
					return
				}
				io.WriteString(w, `{}`)
			}))
			defer server.Close()
			err := testClient(t, server.URL).Heartbeat(context.Background(), State{Status: Idle})
			if code == 429 || code >= 500 {
				if err != nil || calls.Load() != 2 {
					t.Fatalf("transient response was not retried: %v", err)
				}
			} else {
				if err == nil || calls.Load() != 1 {
					t.Fatal("permanent failure was retried")
				}
				if (code == 401 || code == 403) && !errors.Is(err, ErrCredentialRejected) {
					t.Fatal("authentication rejection was not distinct")
				}
				if strings.Contains(err.Error(), "private-database-password") || strings.Contains(err.Error(), testCredential) {
					t.Fatal("response body leaked into error")
				}
			}
		})
	}
}

func TestHeartbeatRetriesAreBoundedAndLogsAreSecretFree(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "private-password "+testCredential)
	}))
	defer server.Close()
	var logs bytes.Buffer
	c := testClient(t, server.URL)
	c.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	err := c.Heartbeat(context.Background(), State{Status: Error, Problem: DatabaseUnavailable})
	var response *ResponseError
	if !errors.As(err, &response) || response.StatusCode != 503 || calls.Load() != 3 {
		t.Fatalf("unbounded or incorrectly classified retries: %v", err)
	}
	for _, secret := range []string{testCredential, "private-password", server.URL, "last_error"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(err.Error(), secret) {
			t.Fatal("sensitive request/response data leaked")
		}
	}
	if !strings.Contains(logs.String(), "heartbeat retry scheduled") {
		t.Fatal("structured retry event missing")
	}
}

func TestHeartbeatDoesNotForwardCredentialOnRedirect(t *testing.T) {
	var forwarded atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	err := testClient(t, source.URL).Heartbeat(context.Background(), State{Status: Idle})
	if err == nil || forwarded.Load() {
		t.Fatal("authenticated redirect was followed")
	}
}

func TestHeartbeatRejectsMalformedSuccessWithoutEchoingBody(t *testing.T) {
	for _, body := range []string{"", "null", "[]", `{} {}`, "<html>private-password</html>", strings.Repeat("x", 4097)} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, body) }))
		err := testClient(t, server.URL).Heartbeat(context.Background(), State{Status: Idle})
		server.Close()
		if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "private-password") {
			t.Fatal("malformed success accepted, retried, or exposed")
		}
	}
}

func TestHeartbeatCancellationInterruptsRequestAndBackoff(t *testing.T) {
	for _, block := range []string{"request", "backoff"} {
		t.Run(block, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if block == "request" {
					// HTTP/1 enables disconnect detection once the request body
					// is consumed; otherwise this fixture can hang Server.Close.
					io.Copy(io.Discard, r.Body)
					close(started)
					<-r.Context().Done()
					return
				}
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			c := testClient(t, server.URL)
			if block == "backoff" {
				c.http.Transport = notifyTransport{base: http.DefaultTransport, notify: started}
			}
			done := make(chan error, 1)
			go func() { done <- c.Heartbeat(ctx, State{Status: Idle}) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
					t.Fatalf("cancellation did not stop retries: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("heartbeat did not cancel promptly")
			}
		})
	}
}

type notifyTransport struct {
	base   http.RoundTripper
	notify chan<- struct{}
}

func (t notifyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	close(t.notify)
	return response, err
}

func TestRetryAfterSurvivesOperationBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	c.budget = 100 * time.Millisecond
	for i := 0; i < 2; i++ {
		err := c.Heartbeat(context.Background(), State{Status: Idle})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("operation was not bounded: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("a new heartbeat ignored the server cooldown")
	}
}

func TestNewRejectsUnsafeCredentialsAndURLs(t *testing.T) {
	for _, credential := range []string{"", "ark_tk_secret", "dac_short", testCredential + "\n"} {
		if _, err := New("https://api.example.com", credential, "dev", nil); err == nil || strings.Contains(err.Error(), credential) && credential != "" && credential != "dac_short" {
			t.Fatal("invalid credential accepted or echoed")
		}
	}
	for _, url := range []string{"http://remote.example.com", "https://user:private-password@api.example.com", "https://api.example.com?token=secret", "https://api.example.com#secret", "https://api.example.com?", "https://api.example.com#"} {
		if _, err := New(url, testCredential, "dev", nil); err == nil || strings.Contains(err.Error(), "private-password") {
			t.Fatal("unsafe API URL accepted or echoed")
		}
	}
}

func TestRetryAfterParsesDatesWithoutOverflow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	if parseRetryAfter("2", now) != 2*time.Second || parseRetryAfter(now.Add(3*time.Second).Format(http.TimeFormat), now) != 3*time.Second || parseRetryAfter("bad", now) != 0 || parseRetryAfter("-1", now) != 0 {
		t.Fatal("Retry-After interpretation failed")
	}
	if parseRetryAfter("9223372036854775807", now) <= 0 {
		t.Fatal("Retry-After overflowed")
	}
}

type failingTransport struct {
	calls *atomic.Int32
	err   error
}

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, t.err
}

func TestTransportRetriesPreserveErrorIdentityWithoutLoggingItsText(t *testing.T) {
	var calls atomic.Int32
	var logs bytes.Buffer
	c := testClient(t, "https://api.example.com")
	original := errors.New("private-proxy-password " + testCredential)
	c.http.Transport = failingTransport{calls: &calls, err: original}
	c.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	err := c.Heartbeat(context.Background(), State{Status: Idle})
	if calls.Load() != 3 || !errors.Is(err, original) {
		t.Fatal("transport retries were unbounded or lost error identity")
	}
	for _, secret := range []string{"private-proxy-password", testCredential, "api.example.com"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatal("raw transport details leaked")
		}
	}
}
