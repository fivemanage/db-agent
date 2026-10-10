package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
)

var ErrCredentialRejected = errors.New("agent credential is invalid, expired, or revoked; stop agent work and resolve access in the dashboard before restarting (do not repeatedly re-enroll)")

var credentialPattern = regexp.MustCompile(`^dac_[A-Za-z0-9_-]{43}$`)

type ResponseError struct {
	StatusCode int
	Operation  string
}

func (e *ResponseError) Error() string {
	operation := e.Operation
	if operation == "" {
		operation = "heartbeat"
	}

	if e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500 {
		return fmt.Sprintf("%s temporarily unavailable (HTTP %d); agent will retry", operation, e.StatusCode)
	}

	return fmt.Sprintf("%s rejected (HTTP %d); check the API endpoint and agent version before restarting", operation, e.StatusCode)
}

// The original transport error remains available to errors.Is/As, but its text
// can contain sensitive URLs or proxy details and must not be logged.
type TransportError struct {
	cause error
}

func (e *TransportError) Error() string {
	return "cannot complete control-plane request; check outbound network access, TLS certificates, and control-plane availability"
}

func (e *TransportError) Unwrap() error { return e.cause }

type Client struct {
	baseURL     string
	credential  string
	version     string
	http        *http.Client
	logger      *slog.Logger
	maxAttempts int
	backoff     time.Duration
	budget      time.Duration
	cooldownMu  sync.Mutex
	cooldown    time.Time
	// Configure before starting workers; the callback runs synchronously and
	// must apply only validated policy without printing local secret material.
	OnOverrides func(context.Context, config.DashboardOverrides) error
}

func New(apiURL, credential, version string, logger *slog.Logger) (*Client, error) {
	if err := ValidateAPIURL(apiURL); err != nil {
		return nil, err
	}

	if !credentialPattern.MatchString(credential) {
		return nil, errors.New("agent_credential must be the issued dac_ credential, not a setup token; check the private config without sharing its contents")
	}

	if strings.TrimSpace(version) != version || version == "" || len(version) > 64 {
		return nil, errors.New("agent version must be nonempty and at most 64 bytes")
	}

	return newClient(apiURL, credential, version, logger), nil
}

func newClient(apiURL, credential, version string, logger *slog.Logger) *Client {
	return &Client{
		baseURL:    strings.TrimRight(apiURL, "/"),
		credential: credential,
		version:    version,
		http: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		logger:      logger,
		maxAttempts: 3,
		backoff:     time.Second,
		budget:      45 * time.Second,
	}
}

func (c *Client) Heartbeat(ctx context.Context, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	payload, err := state.payload(c.version)
	if err != nil {
		return err
	}

	contents, err := c.request(ctx, request{operation: "heartbeat", method: http.MethodPost, path: "/api/v1/agent/heartbeat", credential: c.credential, body: payload})
	if err != nil {
		return err
	}

	return c.applyOverrides(ctx, contents)
}

type request struct {
	operation, method, path, credential, requestID string
	body                                           any
	enrollment                                     bool
	created                                        bool
}

func (c *Client) request(ctx context.Context, call request) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if call.credential == "" && !(call.method == http.MethodGet && call.path == "/api/v1/agent/register") {
		return nil, errors.New("authenticated control-plane request requires a credential")
	}

	var body []byte
	var err error
	if call.body != nil {
		body, err = json.Marshal(call.body)
	}
	if err != nil {
		return nil, errors.New("cannot encode control-plane request")
	}
	if len(body) > 4096 {
		return nil, errors.New("control-plane request exceeds the protocol size limit")
	}

	operation, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()

	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		contents, err, retry, retryAfter := c.send(operation, call, body)
		if err == nil {
			return contents, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if operation.Err() != nil {
			return nil, &TransportError{cause: operation.Err()}
		}
		if !retry || attempt == c.maxAttempts {
			return nil, err
		}

		delay := c.backoff << (attempt - 1)
		if delay > 8*time.Second {
			delay = 8 * time.Second
		}
		delay = delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
		if retryAfter > delay {
			delay = retryAfter
		}

		if c.logger != nil {
			c.logger.WarnContext(ctx, call.operation+" retry scheduled", "attempt", attempt, "delay_ms", delay.Milliseconds())
		}

		if err := wait(operation, delay); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			return nil, &TransportError{cause: err}
		}
	}

	return nil, errors.New("control-plane retry policy is invalid")
}

func (c *Client) send(ctx context.Context, call request, body []byte) ([]byte, error, bool, time.Duration) {
	c.cooldownMu.Lock()
	delay := time.Until(c.cooldown)
	c.cooldownMu.Unlock()
	if delay > 0 {
		if err := wait(ctx, delay); err != nil {
			return nil, &TransportError{cause: err}, true, 0
		}
	}

	req, err := http.NewRequestWithContext(ctx, call.method, c.baseURL+call.path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("cannot create control-plane request; check api_url"), false, 0
	}
	if call.credential != "" {
		req.Header.Set("Authorization", "Bearer "+call.credential)
	}
	if call.requestID != "" {
		req.Header.Set("Idempotency-Key", call.requestID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	response, err := c.http.Do(req)
	if err != nil {
		return nil, &TransportError{cause: err}, true, 0
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		if call.enrollment {
			return nil, ErrInvalidEnrollmentToken, false, 0
		}

		return nil, ErrCredentialRejected, false, 0
	}

	if call.enrollment && response.StatusCode == http.StatusConflict {
		return nil, ErrAlreadyRegistered, false, 0
	}

	if response.StatusCode != http.StatusOK && !((call.enrollment || call.created) && response.StatusCode == http.StatusCreated) {
		err := &ResponseError{StatusCode: response.StatusCode, Operation: call.operation}
		retry := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		if retry && retryAfter > 0 {
			c.cooldownMu.Lock()
			until := time.Now().Add(retryAfter)
			if until.After(c.cooldown) {
				c.cooldown = until
			}
			c.cooldownMu.Unlock()
		}

		return nil, err, retry, retryAfter
	}

	contents, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return nil, &TransportError{cause: err}, true, 0
	}

	var result map[string]json.RawMessage
	if len(contents) > 4096 || json.Unmarshal(contents, &result) != nil || result == nil {
		return nil, errors.New("control plane returned an invalid response; check the final API endpoint (expected a bounded JSON object)"), false, 0
	}

	return contents, nil, false, 0
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		// Values beyond the operation budget still wait for cancellation; cap
		// only to prevent overflowing time.Duration, never to retry sooner.
		if seconds > int64((1<<63-1)/int64(time.Second)) {
			return time.Duration(1<<63 - 1)
		}

		return time.Duration(seconds) * time.Second
	}

	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}

	return 0
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
