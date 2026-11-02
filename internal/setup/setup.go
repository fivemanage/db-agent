// Package setup applies dashboard-driven setup steps without terminal input.
// Database logins are entered locally; this package never receives them.
package setup

import (
	"context"
	"errors"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/diagnostic"
	"github.com/fivemanage/db-agent/internal/key"
)

// Status reports local setup progress. Once a database is saved it creates and
// binds an unacknowledged recovery key, so the user can copy the key file before
// confirming it in the dashboard. The key itself is never displayed or sent.
func Status(path string) (controlplane.SetupStatus, error) {
	status := controlplane.SetupStatus{Protocol: controlplane.SetupProtocol}
	settings, err := config.LoadDiagnostic(path)
	if err != nil {
		return status, err
	}

	if settings.Database.Validate() != nil || len(settings.Database.Databases) == 0 {
		return status, nil
	}
	db := settings.Database
	status.Database = &controlplane.SetupDatabase{Driver: db.Driver, Host: db.Host, Port: db.Port, User: db.User, Name: strings.Join(db.Databases, ", ")}
	status.Restores = settings.Restore.ValidateFor(db) == nil

	var prepareErr error
	if settings.Encryption.KeyID == "" {
		prepareErr = prepareKey(path, settings.Encryption)
		if settings, err = config.LoadDiagnostic(path); err != nil {
			return status, err
		}
	}
	if settings.Encryption.KeyID != "" {
		keyPath, err := config.ResolveKeyPath(path, settings.Encryption.KeyFile)
		if err != nil {
			return status, err
		}
		status.KeyFile = keyPath
		status.KeyAcknowledged = settings.Encryption.KeyAcknowledged
	}
	status.Complete = status.KeyAcknowledged && settings.Validate() == nil

	return status, prepareErr
}

func prepareKey(path string, encryption config.EncryptionConfig) error {
	keyPath, err := config.ResolveKeyPath(path, encryption.KeyFile)
	if err != nil {
		return err
	}

	// A configured key file that is missing must be restored, never replaced.
	k, _, err := key.Ensure(keyPath, encryption.KeyFile == "")
	if err != nil {
		return err
	}

	return config.SaveKeyBinding(path, keyPath, k.ID(), false)
}

// Finish runs every prerequisite check as if the recovery key were acknowledged
// and saves the acknowledgement only when all of them pass. Failed attempts
// change nothing, so the dashboard can retry after the user fixes a problem.
func Finish(ctx context.Context, path string) controlplane.SetupReport {
	settings, err := config.LoadDiagnostic(path)
	if err != nil {
		return failed("Config", err)
	}
	if settings.Encryption.KeyID == "" {
		return failed("Encryption key access", errors.New("the recovery key isn't ready yet; wait for the agent to create it, then try again"))
	}
	keyPath, err := config.ResolveKeyPath(path, settings.Encryption.KeyFile)
	if err != nil {
		return failed("Encryption key access", err)
	}

	candidate := settings
	candidate.Encryption.KeyFile = keyPath
	candidate.Encryption.KeyAcknowledged = true
	report := controlplane.SetupReport{Failed: []controlplane.SetupCheck{}}
	for _, check := range diagnostic.Run(ctx, candidate) {
		if !check.Passed {
			report.Failed = append(report.Failed, controlplane.SetupCheck{Name: check.Name, Skipped: check.Skipped, Message: check.Message})
		}
	}
	if err := ctx.Err(); err != nil {
		return failed("Setup", err)
	}
	if len(report.Failed) > 0 {
		return report
	}

	if err := config.SaveKeyBinding(path, keyPath, settings.Encryption.KeyID, true); err != nil {
		return failed("Encryption key access", err)
	}
	report.Succeeded = true
	return report
}

func failed(name string, err error) controlplane.SetupReport {
	return controlplane.SetupReport{Failed: []controlplane.SetupCheck{{Name: name, Message: err.Error()}}}
}
