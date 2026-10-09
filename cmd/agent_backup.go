package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
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
	}, startup, func(err error) {
		if err != nil {
			fmt.Fprintln(errorOutput, "Automatic backup skipped or failed; check db-agent status and run db-agent test.")
		} else {
			fmt.Fprintln(output, "Automatic backup uploaded and completion acknowledged.")
		}
	})
}
