package diagnostic

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/enrollment"
	"github.com/fivemanage/db-agent/internal/key"
)

func Run(ctx context.Context, settings config.Config) []database.Check {
	return RunReport(ctx, settings).Checks
}

type Report struct {
	Checks  []database.Check
	Driver  string
	Version string
}

func RunReport(ctx context.Context, settings config.Config) Report {
	configCheck := database.Check{Name: "Config", Passed: true, Message: "Configuration is valid."}
	if err := settings.Validate(); err != nil {
		configCheck.Passed, configCheck.Message = false, err.Error()+"; complete local setup in the config file."
	}

	report := database.Inspect(ctx, settings.Database)
	_, tooling := database.DetectTools(ctx, report.Driver, report.Version)
	checks := append([]database.Check{configCheck}, report.Checks...)

	keyCheck := CheckKey(settings.Encryption.KeyFile)
	if keyCheck.Passed {
		k, err := key.Load(settings.Encryption.KeyFile)
		if err != nil || !settings.Encryption.KeyAcknowledged || k.ID() != settings.Encryption.KeyID {
			keyCheck.Passed = false
			keyCheck.Message = "Recovery key must match its saved fingerprint and be acknowledged; run db-agent configure --key."
		}
	}

	return Report{Checks: append(checks, tooling, keyCheck, CheckAPI(ctx, settings.APIURL)), Driver: report.Driver, Version: report.Version}
}

func CheckKey(path string) database.Check {
	check := database.Check{Name: "Encryption key access"}
	if path == "" {
		check.Message = "Set up and acknowledge your local recovery key with db-agent configure --key."
		return check
	}

	if _, err := key.Load(path); err != nil {
		check.Message = "Recovery key must be a valid private dbak1_ file; check its path, permissions, and service-account access. Restore the original key rather than generating a replacement."
		return check
	}

	check.Passed, check.Message = true, "Local recovery key format and private file permissions are valid."

	return check
}

// Reachability deliberately sends neither database settings nor an agent
// credential. Authentication/heartbeat health is a separate protocol check.
func CheckAPI(ctx context.Context, apiURL string) database.Check {
	check := database.Check{Name: "Control-plane reachability"}
	if err := enrollment.ValidateAPIURL(apiURL); err != nil {
		check.Message = err.Error()
		return check
	}

	probeURL := strings.TrimRight(apiURL, "/") + "/api/v1/agent/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		check.Message = "Invalid API URL; check api_url in local config."
		return check
	}

	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		check.Message = "Cannot reach the control plane; check api_url, outbound network access, TLS certificates, and firewall settings."
		return check
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 && response.StatusCode < 400 {
		//check.Message = "Control plane redirected the request; configure the final API base URL (redirects are not followed)."
		check.Message = fmt.Sprintf("Control plane redirected the request; configure the final API base URL (%s).", apiURL)
		return check
	}

	if response.StatusCode >= 500 {
		check.Message = fmt.Sprintf("Control plane returned HTTP %d; retry later or check the API service.", response.StatusCode)
		return check
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed {
		check.Message = "Agent API endpoint not found; configure api_url as the dashboard base URL without /api or /api/v1, and verify the agent routes are deployed."
		return check
	}

	check.Passed, check.Message = true, fmt.Sprintf("API host reached (HTTP %d); agent authentication is not checked by this probe.", response.StatusCode)

	return check
}
