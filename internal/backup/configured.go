package backup

import (
	"context"
	"errors"
	"io"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/dump"
	"github.com/fivemanage/db-agent/internal/key"
)

// Prepare validates only local prerequisites; an API outage must not prevent
// building the bounded encrypted spool. Dump/client credentials stay local.
func Prepare(ctx context.Context, path string, settings config.Config) (Plan, error) {
	if err := settings.Validate(); err != nil {
		return Plan{}, err
	}
	keyPath, err := config.ResolveKeyPath(path, settings.Encryption.KeyFile)
	if err != nil {
		return Plan{}, err
	}
	k, err := key.Load(keyPath)
	if err != nil || k.ID() != settings.Encryption.KeyID {
		return Plan{}, errors.New("restore the original private recovery key matching its saved fingerprint; run db-agent test")
	}
	report := database.Inspect(ctx, settings.Database)
	if !report.Passed() {
		return Plan{}, errors.New("local database prerequisites failed; run db-agent test before retrying")
	}
	if report.Driver != settings.Database.Driver {
		return Plan{}, errors.New("configured database driver does not match the local server; run db-agent configure before retrying")
	}
	tool, check := database.DetectTools(ctx, report.Driver, report.Version)
	if !check.Passed {
		return Plan{}, errors.New(check.Message)
	}
	timeout, err := settings.Backup.DumpDuration()
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Key: k, Compression: artifact.Compression(settings.Backup.Compression),
		Dump: func(ctx context.Context, dst io.Writer) error {
			return dump.Run(ctx, dst, settings.Database, tool, dump.Options{Timeout: timeout, ThrottleIO: settings.Backup.ThrottleIO})
		},
	}, nil
}
