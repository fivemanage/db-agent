package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/spf13/cobra"
)

var agentCmd = &cobra.Command{
	Use:    "agent",
	Short:  "Run the agent in the foreground",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, err := config.LoadAgent(configPath)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		fmt.Fprintln(cmd.OutOrStdout(), "Agent started. Press Ctrl+C to stop.")
		if !settings.AutoUpdate {
			<-ctx.Done()
		} else {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				if version != "dev" {
					updated, err := checkAndMaybeInstall(ctx, settings, true, true)
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "Update check failed: %v\n", err)
					} else if updated {
						fmt.Fprintln(cmd.OutOrStdout(), "Update staged; agent stopping for replacement.")
						return nil
					}
				}
				select {
				case <-ctx.Done():
					fmt.Fprintln(cmd.OutOrStdout(), "Agent stopped.")
					return nil
				case <-ticker.C:
				}
			}
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Agent stopped.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(agentCmd)
}
