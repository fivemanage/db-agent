package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/diagnostic"
)

const setupHeartbeatSyncTimeout = 5 * time.Second

func applyAgentOverrides(ctx context.Context, path string, scheduler *backup.Scheduler, overrides config.DashboardOverrides) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := config.SaveDashboardOverrides(path, overrides); err != nil {
		return err
	}
	if overrides.Backup.Schedule != nil {
		return scheduler.Update(*overrides.Backup.Schedule)
	}
	return nil
}

func runScheduledBackups(ctx context.Context, path string, scheduler *backup.Scheduler, runner *backup.Runner, client *controlplane.Client, ready bool, output, errorOutput io.Writer) error {
	snapshot, err := runner.Snapshot()
	if err != nil {
		return err
	}
	startup := ready && snapshot.Latest == nil
	return scheduler.Run(ctx, func(ctx context.Context) error {
		return runBackupNow(ctx, path, client)
	}, startup, func(err error) {
		reportAutomaticBackup(err, output, errorOutput)
	})
}

// runBackupNow uses the current config and the shared whole-run lock, like a
// scheduled trigger.
func runBackupNow(ctx context.Context, path string, client *controlplane.Client) error {
	effective, err := config.LoadAgent(path)
	if err != nil {
		return err
	}
	runner, err := newBackupRunner(path, effective, client)
	if err != nil {
		return err
	}
	return runner.Run(ctx, func(ctx context.Context) (backup.Plan, error) {
		return backup.Prepare(ctx, path, effective)
	})
}

func reportAutomaticBackup(err error, output, errorOutput io.Writer) {
	if errors.Is(err, backup.ErrConfirmationQueued) {
		fmt.Fprintln(output, "Encrypted backup uploaded; waiting to confirm completion. The agent will retry automatically.")
	} else if err != nil {
		fmt.Fprintln(errorOutput, "Automatic backup skipped or failed; check db-agent status and run db-agent test.")
	} else {
		fmt.Fprintln(output, "Automatic backup uploaded and completion acknowledged.")
	}
}

// refreshDashboardSetupReadiness reloads diagnostics and flushes one heartbeat
// before setup reports complete so the dashboard never sees a stale setup error.
func refreshDashboardSetupReadiness(ctx context.Context, path string, client *controlplane.Client, readiness *heartbeatReadiness, errorOutput io.Writer) {
	settings, err := config.LoadAgent(path)
	if err != nil {
		fmt.Fprintf(errorOutput, "Setup finished, but the config could not be reloaded: %v\n", err)
		return
	}
	readiness.set(diagnostic.RunReport(ctx, settings))
	if ctx.Err() != nil {
		return
	}

	syncCtx, cancel := context.WithTimeout(ctx, setupHeartbeatSyncTimeout)
	defer cancel()
	client.SyncHeartbeat(syncCtx)
}

// startFirstBackupAfterSetup launches the initial backup once setup completion
// has been reported and refreshed prerequisites pass.
func startFirstBackupAfterSetup(ctx context.Context, path string, client *controlplane.Client, readiness *heartbeatReadiness, output, errorOutput io.Writer) {
	if readiness.get().Status != controlplane.Idle {
		fmt.Fprintln(errorOutput, "Setup finished, but prerequisite checks no longer pass; the first backup was not started. Run db-agent test.")
		return
	}

	fmt.Fprintln(output, "Setup finished. Starting the first backup.")
	err := runBackupNow(ctx, path, client)
	if ctx.Err() == nil {
		reportAutomaticBackup(err, output, errorOutput)
	}
}
