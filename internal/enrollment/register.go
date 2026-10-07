package enrollment

import (
	"context"
	"errors"
	"os"
	"runtime"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
)

var ErrInvalidToken = controlplane.ErrInvalidEnrollmentToken
var ErrAlreadyRegistered = controlplane.ErrAlreadyRegistered

func metadata(version string) (controlplane.RegistrationMetadata, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return controlplane.RegistrationMetadata{}, errors.New("cannot read local hostname")
	}

	return controlplane.RegistrationMetadata{AgentVersion: version, Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH}, nil
}

// Register keeps one candidate credential across all retries in this invocation.
// The install command uses Enroll to preserve it across process restarts too.
func Register(ctx context.Context, apiURL, token, version string) (string, error) {
	client, err := controlplane.NewEnrollment(apiURL, nil)
	if err != nil {
		return "", err
	}

	candidate, err := controlplane.GenerateCredential()
	if err != nil {
		return "", err
	}

	m, err := metadata(version)
	if err != nil {
		return "", err
	}

	result, err := client.Register(ctx, token, candidate, m)

	return result.AgentCredential, err
}

func Enroll(ctx context.Context, configPath, apiURL, token, version string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if err := ValidateAPIURL(apiURL); err != nil {
		return "", err
	}

	if _, err := os.Lstat(config.ResolvePath(configPath)); err == nil {
		return "", errors.New("config already exists; enrollment will not overwrite it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cannot check config before enrollment")
	}

	pending, err := config.LoadPendingEnrollment(configPath)
	if errors.Is(err, os.ErrNotExist) {
		credential, err := controlplane.GenerateCredential()
		if err != nil {
			return "", err
		}

		pending = config.PendingEnrollment{APIURL: apiURL, Credential: credential}
		if err := config.SavePendingEnrollment(configPath, pending); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if pending.APIURL != apiURL {
		return "", errors.New("pending enrollment uses a different API URL; do not replace or rotate the pending credential")
	}

	client, err := controlplane.NewEnrollment(apiURL, nil)
	if err != nil {
		return "", err
	}

	m, err := metadata(version)
	if err != nil {
		return "", err
	}

	result, err := client.Register(ctx, token, pending.Credential, m)
	if err != nil {
		return "", err
	}

	if err := config.SaveEnrollment(configPath, apiURL, result.AgentCredential); err != nil {
		return "", errors.New("enrollment confirmed, but saving config failed; preserve the pending file and rerun install to recover without another token")
	}

	if result.Overrides != nil {
		if err := config.SaveDashboardOverrides(configPath, *result.Overrides); err != nil {
			return "", err
		}
	}

	if err := config.ClearPendingEnrollment(configPath); err != nil {
		return "", errors.New("enrollment saved, but pending file cleanup failed; check local directory permissions")
	}

	return result.AgentCredential, nil
}

func Recover(ctx context.Context, configPath, apiURL, version string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	if _, err := os.Lstat(config.ResolvePath(configPath)); err == nil {
		return false, errors.New("config already exists; recovery will not overwrite it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("cannot check config before recovery")
	}

	pending, err := config.LoadPendingEnrollment(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pending.APIURL != apiURL {
		return false, errors.New("pending enrollment uses a different API URL; do not replace it")
	}

	client, err := controlplane.New(apiURL, pending.Credential, version, nil)
	if err != nil {
		return false, err
	}

	var overrides *config.DashboardOverrides
	client.OnOverrides = func(_ context.Context, value config.DashboardOverrides) error { overrides = &value; return nil }
	err = client.Heartbeat(ctx, controlplane.State{Status: controlplane.Error, Problem: controlplane.SetupIncomplete})
	if errors.Is(err, controlplane.ErrCredentialRejected) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if err := config.SaveEnrollment(configPath, apiURL, pending.Credential); err != nil {
		return false, errors.New("issued pending credential was confirmed, but saving config failed; preserve the pending file and retry")
	}

	if overrides != nil {
		if err := config.SaveDashboardOverrides(configPath, *overrides); err != nil {
			return false, err
		}
	}

	if err := config.ClearPendingEnrollment(configPath); err != nil {
		return false, errors.New("recovered enrollment saved, but pending file cleanup failed")
	}

	return true, nil
}

func ValidateAPIURL(apiURL string) error { return controlplane.ValidateAPIURL(apiURL) }
