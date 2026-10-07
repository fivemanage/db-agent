package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
)

var ErrInvalidEnrollmentToken = errors.New("setup token is invalid, expired, revoked, or already used by another enrollment; use the original token to resume, or check the dashboard")
var ErrAlreadyRegistered = errors.New("this database already has an agent; check the dashboard before trying again")

const RegistrationProtocol = "client-credential-v1"

type DatabaseMetadata struct {
	Driver  string `json:"db_driver,omitempty" yaml:"db_driver,omitempty"`
	Version string `json:"db_version,omitempty" yaml:"db_version,omitempty"`
}

var databaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(?:\.[0-9]+)?$`)

func (m DatabaseMetadata) validate() error {
	if m.Driver == "" && m.Version == "" {
		return nil
	}
	if (m.Driver != "mysql" && m.Driver != "mariadb") || !databaseVersionPattern.MatchString(m.Version) || len(m.Version) > 32 {
		return errors.New("database metadata must contain a known driver and numeric server version, never connection details")
	}
	return nil
}

type RegistrationMetadata struct {
	AgentVersion string `json:"agent_version"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	DatabaseMetadata
}

type RegistrationResult struct {
	AgentCredential string                     `json:"agent_credential"`
	Overrides       *config.DashboardOverrides `json:"-"`
}

func GenerateCredential() (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", errors.New("cannot generate secure agent credential")
	}
	return "dac_" + base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

func NewEnrollment(apiURL string, logger *slog.Logger) (*Client, error) {
	if err := ValidateAPIURL(apiURL); err != nil {
		return nil, err
	}
	return newClient(apiURL, "", "", logger), nil
}

func (c *Client) Register(ctx context.Context, token, proposedCredential string, metadata RegistrationMetadata) (RegistrationResult, error) {
	if token == "" || strings.ContainsAny(token, "\r\n") || strings.TrimSpace(token) != token {
		return RegistrationResult{}, errors.New("setup token is required and must be a single line")
	}
	if !credentialPattern.MatchString(proposedCredential) {
		return RegistrationResult{}, errors.New("enrollment requires a securely generated candidate credential")
	}
	if metadata.AgentVersion == "" || len(metadata.AgentVersion) > 64 || metadata.Hostname == "" || len(metadata.Hostname) > 255 || metadata.OS == "" || len(metadata.OS) > 32 || metadata.Arch == "" || len(metadata.Arch) > 32 {
		return RegistrationResult{}, errors.New("registration metadata is incomplete or too long")
	}
	if err := metadata.DatabaseMetadata.validate(); err != nil {
		return RegistrationResult{}, err
	}
	// Capability negotiation happens before sending the one-time token. Older
	// servers must not consume it while silently ignoring replay protection.
	capabilities, err := c.request(ctx, request{operation: "registration discovery", method: http.MethodGet, path: "/v1/agent/register"})
	if err != nil {
		return RegistrationResult{}, err
	}
	var capability struct {
		Protocol string `json:"registration_protocol"`
	}
	if json.Unmarshal(capabilities, &capability) != nil || capability.Protocol != RegistrationProtocol {
		return RegistrationResult{}, errors.New("control plane does not support safe enrollment retries; deploy the matching registration protocol before installing")
	}
	body := struct {
		RegistrationMetadata
		Credential string `json:"agent_credential"`
	}{metadata, proposedCredential}
	contents, err := c.request(ctx, request{operation: "registration", method: http.MethodPost, path: "/v1/agent/register", credential: token, body: body, enrollment: true})
	if err != nil {
		return RegistrationResult{}, err
	}
	var result RegistrationResult
	if json.Unmarshal(contents, &result) != nil || result.AgentCredential != proposedCredential {
		return RegistrationResult{}, errors.New("registration response did not confirm the pending credential; keep the pending enrollment file and contact support before re-enrolling")
	}
	result.Overrides, err = responseOverrides(contents)
	return result, err
}
