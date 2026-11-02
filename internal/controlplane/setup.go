package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const SetupProtocol = 1

const SetupFinish = "finish"

// SetupDatabase identifies the saved backup login for the dashboard. Passwords
// and connection strings are never part of setup status.
type SetupDatabase struct {
	Driver string `json:"driver"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	Name   string `json:"name"`
}

type SetupStatus struct {
	Protocol        int            `json:"protocol"`
	Database        *SetupDatabase `json:"database,omitempty"`
	Restores        bool           `json:"restores"`
	KeyFile         string         `json:"key_file,omitempty"`
	KeyAcknowledged bool           `json:"key_acknowledged"`
	Complete        bool           `json:"complete"`
}

type SetupRequest struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type SetupPollResponse struct {
	Protocol int           `json:"protocol"`
	Request  *SetupRequest `json:"request"`
}

type SetupCheck struct {
	Name    string `json:"name"`
	Skipped bool   `json:"skipped,omitempty"`
	Message string `json:"message"`
}

// SetupReport lists only the checks that did not pass, which keeps the report
// within the protocol size limit.
type SetupReport struct {
	Succeeded bool         `json:"succeeded"`
	Failed    []SetupCheck `json:"failed"`
}

const setupTextLimit = 400

func validSetupText(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit && !strings.ContainsRune(value, 0)
}

func (s SetupStatus) validate() error {
	if s.Protocol != SetupProtocol {
		return errors.New("invalid setup status protocol")
	}
	if db := s.Database; db != nil {
		if db.Driver != "mysql" && db.Driver != "mariadb" || db.Port < 1 || db.Port > 65535 ||
			!validSetupText(db.Host, 255) || !validSetupText(db.User, 128) || !validSetupText(db.Name, 1024) || db.Host == "" || db.User == "" || db.Name == "" {
			return errors.New("invalid setup database summary")
		}
	}
	if s.KeyFile != "" && (!validSetupText(s.KeyFile, 1024) || !filepath.IsAbs(s.KeyFile)) {
		return errors.New("setup status key path must be absolute")
	}
	if s.Complete && (s.Database == nil || !s.KeyAcknowledged) {
		return errors.New("complete setup requires a database and an acknowledged recovery key")
	}
	return nil
}

func (c *Client) PollSetup(ctx context.Context, status SetupStatus) (SetupPollResponse, error) {
	if err := status.validate(); err != nil {
		return SetupPollResponse{}, err
	}
	data, err := c.request(ctx, request{operation: "setup poll", method: http.MethodPost, path: "/api/v1/agent/setup/poll", credential: c.credential, body: status})
	if err != nil {
		return SetupPollResponse{}, err
	}
	var result SetupPollResponse
	if json.Unmarshal(data, &result) != nil || result.Protocol != SetupProtocol {
		return SetupPollResponse{}, errors.New("invalid setup poll response")
	}
	if result.Request != nil && (!resourcePattern.MatchString(result.Request.ID) || result.Request.Kind != SetupFinish) {
		return SetupPollResponse{}, errors.New("setup poll returned an unsupported request")
	}
	return result, nil
}

func (c *Client) ReportSetup(ctx context.Context, id string, report SetupReport) error {
	if !resourcePattern.MatchString(id) || report.Succeeded != (len(report.Failed) == 0) || len(report.Failed) > 16 {
		return errors.New("setup report requires a stable request ID and failed checks only on failure")
	}
	body := SetupReport{Succeeded: report.Succeeded, Failed: make([]SetupCheck, len(report.Failed))}
	for i, check := range report.Failed {
		if !validSetupText(check.Name, 64) || check.Name == "" {
			return errors.New("invalid setup check name")
		}
		body.Failed[i] = SetupCheck{Name: check.Name, Skipped: check.Skipped, Message: truncateSetupText(check.Message)}
	}
	_, err := c.request(ctx, request{operation: "setup report", method: http.MethodPost, path: "/api/v1/agent/setup/" + id + "/report", credential: c.credential, body: body})
	return err
}

func truncateSetupText(value string) string {
	value = strings.ToValidUTF8(strings.ReplaceAll(value, "\x00", ""), "")
	if len(value) <= setupTextLimit {
		return value
	}
	cut := setupTextLimit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
