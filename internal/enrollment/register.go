package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

var (
	ErrInvalidToken      = errors.New("setup token is invalid, expired, or already used; generate a new token in the dashboard")
	ErrAlreadyRegistered = errors.New("this database already has an agent; check the dashboard before trying again")
)

type registrationRequest struct {
	AgentVersion string `json:"agent_version"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
}

type registrationResponse struct {
	AgentCredential string `json:"agent_credential"`
}

func Register(ctx context.Context, apiURL, token, version string) (string, error) {
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("setup token is required and must be a single line")
	}

	if err := ValidateAPIURL(apiURL); err != nil {
		return "", err
	}

	base, _ := url.Parse(apiURL)

	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("read hostname: %w", err)
	}

	body, err := json.Marshal(registrationRequest{AgentVersion: version, Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		return "", fmt.Errorf("encode registration request: %w", err)
	}

	endpoint := strings.TrimRight(base.String(), "/") + "/v1/agent/register"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create registration request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("contact control plane: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrInvalidToken
	case http.StatusConflict:
		return "", ErrAlreadyRegistered
	default:
		return "", fmt.Errorf("registration failed (HTTP %d)", resp.StatusCode)
	}

	var result registrationResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return "", errors.New("control plane returned an invalid registration response")
	}

	if strings.TrimSpace(result.AgentCredential) == "" || strings.ContainsAny(result.AgentCredential, "\r\n") {
		return "", errors.New("control plane did not return a valid agent credential")
	}
	return result.AgentCredential, nil
}

func ValidateAPIURL(apiURL string) error {
	base, err := url.Parse(apiURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("API URL must be an HTTPS base URL without credentials, query, or fragment")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && isLoopback(base.Hostname())) {
		return errors.New("API URL must use HTTPS (HTTP is allowed only on loopback for local development)")
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
