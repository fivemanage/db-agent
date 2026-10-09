package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/transfer"
	"github.com/spf13/cobra"
)

func newBackupCommand() *cobra.Command {
	var now bool
	command := &cobra.Command{
		Use: "backup --now", Short: "Back up the configured databases now", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !now {
				return errors.New("use db-agent backup --now to start a manual backup")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runConfiguredBackup(ctx, config.ResolvePath(configPath), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	command.Flags().BoolVar(&now, "now", false, "start a backup immediately")
	return command
}

func newBackupRunner(path string, settings config.Config, client *controlplane.Client) (*backup.Runner, error) {
	options := transfer.Options{}
	if settings.Backup.ThrottleIO {
		options.BytesPerSecond = 4 << 20
	}
	dir := filepath.Join(filepath.Dir(path), "backup-transfer")
	uploader, err := transfer.New(dir, client, options)
	if err != nil {
		return nil, err
	}
	return backup.New(dir, uploader)
}

func runConfiguredBackup(ctx context.Context, path string, output, errorOutput io.Writer) error {
	settings, err := config.LoadAgent(path)
	if err != nil {
		return err
	}
	client, err := controlplane.New(settings.APIURL, settings.AgentCredential, version, slog.New(slog.NewJSONHandler(errorOutput, nil)))
	if err != nil {
		return err
	}
	runner, err := newBackupRunner(path, settings, client)
	if err != nil {
		return err
	}

	err = runManualBackup(ctx, runner, client, func(ctx context.Context) (backup.Plan, error) {
		return backup.Prepare(ctx, path, settings)
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "Backup uploaded and completion acknowledged.")
	return nil
}

type backupHeartbeat interface {
	RunHeartbeat(context.Context, func() controlplane.State, func(controlplane.Result)) error
	Heartbeat(context.Context, controlplane.State) error
}

func runManualBackup(parent context.Context, runner *backup.Runner, client backupHeartbeat, prepare func(context.Context) (backup.Plan, error)) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	current := func() controlplane.State { return runner.HeartbeatState(controlplane.State{Status: controlplane.Idle}) }
	done := make(chan error, 1)
	go func() {
		err := client.RunHeartbeat(ctx, current, nil)
		if err != nil {
			cancel()
		}
		done <- err
	}()

	err := runner.Run(ctx, prepare)
	cancel()
	heartbeatErr := <-done
	if heartbeatErr != nil {
		return errors.Join(heartbeatErr, err)
	}
	if parent.Err() == nil && !errors.Is(err, backup.ErrRunning) && !errors.Is(err, transfer.ErrBusy) {
		// Join the periodic worker first so an older in-flight heartbeat cannot
		// overwrite the final state. Transient liveness failures don't undo a PUT.
		if heartbeatErr := client.Heartbeat(parent, current()); heartbeatErr != nil && errors.Is(heartbeatErr, controlplane.ErrCredentialRejected) {
			return errors.Join(heartbeatErr, err)
		}
	}
	return err
}

func init() { rootCmd.AddCommand(newBackupCommand()) }
