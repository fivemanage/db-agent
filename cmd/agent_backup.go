package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/diagnostic"
)

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
	if err != nil {
		fmt.Fprintln(errorOutput, "Automatic backup skipped or failed; check db-agent status and run db-agent test.")
	} else {
		fmt.Fprintln(output, "Automatic backup uploaded and completion acknowledged.")
	}
}

// finishDashboardSetup refreshes heartbeat readiness after setup completes and
// starts the first backup without a service restart.
func finishDashboardSetup(ctx context.Context, path string, client *controlplane.Client, readiness *heartbeatReadiness, output, errorOutput io.Writer) {
	settings, err := config.LoadAgent(path)
	if err != nil {
		fmt.Fprintf(errorOutput, "Setup finished, but the config could not be reloaded: %v\n", err)
		return
	}
	readiness.set(diagnostic.RunReport(ctx, settings))
	if ctx.Err() != nil {
		return
	}
	if readiness.get().Status != controlplane.Idle {
		fmt.Fprintln(errorOutput, "Setup finished, but prerequisite checks no longer pass; the first backup was not started. Run db-agent test.")
		return
	}

	fmt.Fprintln(output, "Setup finished. Starting the first backup.")
	err = runBackupNow(ctx, path, client)
	if ctx.Err() == nil {
		reportAutomaticBackup(err, output, errorOutput)
	}
}
