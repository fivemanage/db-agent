package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/fivemanage/db-agent/internal/config"
	services "github.com/fivemanage/db-agent/internal/service"
	manager "github.com/kardianos/service"
	"github.com/spf13/cobra"
)

func serviceCommand(action string) *cobra.Command {
	return &cobra.Command{
		Use:   action,
		Short: map[string]string{"start": "Start the installed agent service", "stop": "Stop the agent service gracefully", "uninstall": "Remove the service, keeping config, keys, backups, and logs"}[action],
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return controlService(config.ResolvePath(configPath), cmd.OutOrStdout(), services.NewController, action)
		},
	}
}

func controlService(path string, output io.Writer, factory func(*manager.Config) (services.Controller, error), actions ...string) error {
	cfg, err := services.Config(path)
	if err != nil {
		return err
	}

	svc, err := factory(cfg)
	if err != nil {
		return err
	}

	for _, action := range actions {
		message, err := services.Control(svc, action)
		if err != nil {
			return err
		}

		fmt.Fprintln(output, message)
	}

	return nil
}

var serviceRunCmd = &cobra.Command{
	Use:    "service-run",
	Short:  "Run under the OS service manager",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if runtime.GOOS == "windows" && manager.Interactive() {
			return errors.New("service-run must be launched by Windows Service; use db-agent agent to run in the foreground")
		}

		path := config.ResolvePath(configPath)
		cfg, err := services.Config(path)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		return services.Run(ctx, cfg, func(ctx context.Context, svc manager.Service) error {
			output, errorOutput := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if runtime.GOOS == "windows" {
				logger, err := svc.SystemLogger(nil)
				if err != nil {
					return fmt.Errorf("open Windows service event log: %w", err)
				}
				output = serviceLogWriter{log: logger.Info}
				errorOutput = serviceLogWriter{log: logger.Error}
			}

			// launchd replacement/restart is not implemented by the signed updater.
			err := runConfiguredAgent(ctx, path, output, errorOutput, version != "dev" && runtime.GOOS != "darwin")
			if err != nil {
				fmt.Fprintf(errorOutput, "Agent stopped: %v\n", err)
			}

			return err
		})
	},
}

type serviceLogWriter struct{ log func(...interface{}) error }

func (w serviceLogWriter) Write(p []byte) (int, error) {
	if message := strings.TrimSpace(string(p)); message != "" {
		if err := w.log(message); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

func init() {
	rootCmd.AddCommand(serviceCommand("start"), serviceCommand("stop"), serviceCommand("uninstall"), serviceRunCmd)
}
