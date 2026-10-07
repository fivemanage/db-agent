package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/fivemanage/db-agent/internal/agent"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/diagnostic"
	"github.com/spf13/cobra"
)

var agentCmd = &cobra.Command{
	Use:    "agent",
	Short:  "Run the agent in the foreground",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		return agent.Run(ctx, func(ctx context.Context) error {
			return runConfiguredAgent(ctx, config.ResolvePath(configPath), cmd.OutOrStdout(), cmd.ErrOrStderr(), version != "dev")
		})
	},
}

func runConfiguredAgent(ctx context.Context, path string, output, errorOutput io.Writer, autoUpdate bool) error {
	settings, err := config.LoadAgent(path)
	if err != nil {
		return err
	}

	var outputMu sync.Mutex
	output = lockedWriter{writer: output, mu: &outputMu}
	errorOutput = lockedWriter{writer: errorOutput, mu: &outputMu}

	client, err := controlplane.New(settings.APIURL, settings.AgentCredential, version, slog.New(slog.NewJSONHandler(errorOutput, nil)))
	if err != nil {
		return err
	}

	checks := diagnostic.RunReport(ctx, settings)
	printChecks(output, checks.Checks)
	if ctx.Err() != nil {
		return nil
	}

	state := startupHeartbeatState(checks.Checks)
	if checks.Driver != "" && checks.Version != "" {
		state.DatabaseMetadata = controlplane.DatabaseMetadata{Driver: checks.Driver, Version: checks.Version}
	}

	client.OnOverrides = func(ctx context.Context, overrides config.DashboardOverrides) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		return config.SaveDashboardOverrides(path, overrides)
	}

	return runAgent(ctx, output, errorOutput, agentWork{
		heartbeats: func(ctx context.Context, report func(controlplane.Result)) error {
			return client.RunHeartbeat(ctx, func() controlplane.State { return state }, report)
		},
		update: func(ctx context.Context) (bool, error) {
			effective, err := config.LoadAgent(path)
			if err != nil || !effective.AutoUpdate {
				return false, err
			}

			return checkAndMaybeInstall(ctx, effective, true, true)
		},
	}, autoUpdate)
}

type lockedWriter struct {
	writer io.Writer
	mu     *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.writer.Write(p)
}

type agentWork struct {
	heartbeats func(context.Context, func(controlplane.Result)) error
	update     func(context.Context) (bool, error)
}

type updateResult struct {
	installed bool
	err       error
}

func startupHeartbeatState(checks []database.Check) controlplane.State {
	if len(checks) == 0 {
		return controlplane.State{Status: controlplane.Error, Problem: controlplane.SetupIncomplete}
	}

	for _, check := range checks {
		// The unauthenticated base-URL probe can redirect even when the actual
		// heartbeat route works. Liveness errors belong to its own transport.
		if check.Name != "Control-plane reachability" && !check.Passed {
			return controlplane.State{Status: controlplane.Error, Problem: controlplane.SetupIncomplete}
		}
	}

	return controlplane.State{Status: controlplane.Idle}
}

func runAgent(ctx context.Context, output, errorOutput io.Writer, work agentWork, autoUpdate bool) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()

	reports := make(chan controlplane.Result, 1)
	heartbeatDone := make(chan error, 1)
	updates := make(chan updateResult, 1)

	workers.Add(1)
	go func() {
		defer workers.Done()
		err := work.heartbeats(ctx, func(result controlplane.Result) {
			select {
			case reports <- result:
			case <-ctx.Done():
			}
		})
		heartbeatDone <- err
	}()

	var previous controlplane.Connection
	updaterStarted := false
	fmt.Fprintln(output, "Agent started. A heartbeat confirms connectivity, not a completed backup.")

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(output, "Agent stopped.")
			return nil
		case err := <-heartbeatDone:
			if err == nil {
				fmt.Fprintln(output, "Agent stopped.")
			}

			return err
		case result := <-reports:
			if result.Connection != previous {
				if result.Connection == controlplane.Connected {
					fmt.Fprintln(output, "Heartbeat accepted. Agent connection established.")
				} else if result.Connection == controlplane.Unreachable {
					fmt.Fprintf(errorOutput, "Agent connection unavailable: %v\n", result.Err)
				}
				previous = result.Connection
			}

			// A rejected initial credential must not start agent work. Public,
			// signed update checks can still run during a transient API outage.
			if autoUpdate && !updaterStarted && (result.Connection == controlplane.Connected || result.Connection == controlplane.Unreachable) {
				updaterStarted = true
				workers.Add(1)
				go func() {
					defer workers.Done()
					runAgentUpdates(ctx, work.update, updates)
				}()
			}
		case result := <-updates:
			if result.err != nil {
				fmt.Fprintf(errorOutput, "Update check failed: %v\n", result.err)
			} else if result.installed {
				fmt.Fprintln(output, "Update staged; agent stopping for replacement.")
				return nil
			}
		}
	}
}

func runAgentUpdates(ctx context.Context, check func(context.Context) (bool, error), reports chan<- updateResult) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		installed, err := check(ctx)
		if ctx.Err() != nil {
			return
		}

		select {
		case reports <- updateResult{installed: installed, err: err}:
		case <-ctx.Done():
			return
		}

		if installed && err == nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func init() {
	rootCmd.AddCommand(agentCmd)
}
