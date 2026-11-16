package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
)

func printRestoreUpgradeNotice(ctx context.Context, output io.Writer, settings config.Config,
	check func(context.Context, config.DatabaseConfig, config.RestoreConfig) error,
) {
	if !settings.Restore.Authorized || ctx.Err() != nil {
		return
	}
	err := check(ctx, settings.Database, settings.Restore)
	if err == nil || ctx.Err() != nil {
		return
	}
	var missing *database.RestoreTriggerGrantError
	if !errors.As(err, &missing) {
		fmt.Fprintln(output, "Could not check trigger restore permissions. Verify your local restore connection and rerun db-agent update check. This does not block the update.")
		return
	}
	fmt.Fprintln(output, "Trigger restores need a one-time database permission update.")
	fmt.Fprintln(output, "Run this SQL as a database administrator on the local MySQL/MariaDB server:")
	fmt.Fprintln(output, missing.SQL)
	fmt.Fprintln(output, "Backups and table/view-only restores do not need this extra permission.")
	fmt.Fprintln(output, "The agent does not run this SQL. Do not delete database users, config, or encryption keys when updating.")
}
