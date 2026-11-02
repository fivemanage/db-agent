package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fivemanage/db-agent/internal/agent"
	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/diagnostic"
	"github.com/fivemanage/db-agent/internal/restorejob"
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
	scheduler, err := backup.NewScheduler(filepath.Join(filepath.Dir(path), "backup-transfer"), settings.Backup.Schedule)
	if err != nil {
		return err
	}

	checks := diagnostic.RunReport(ctx, settings)
	printChecks(output, checks.Checks)
	if ctx.Err() != nil {
		return nil
	}

	readiness := &heartbeatReadiness{}
	readiness.set(checks)
	startupReady := readiness.get().Status == controlplane.Idle

	client.OnOverrides = func(ctx context.Context, overrides config.DashboardOverrides) error {
		return applyAgentOverrides(ctx, path, scheduler, overrides)
	}

	runner, err := newBackupRunner(path, settings, client)
	if err != nil {
		return err
	}
	restores, err := restorejob.NewWorker(path, client, restorejob.Options{})
	if err != nil {
		return err
	}

	return runAgent(ctx, output, errorOutput, agentWork{
		heartbeats: func(ctx context.Context, report func(controlplane.Result)) error {
			return runHeartbeatTransfers(ctx, func(ctx context.Context, report func(controlplane.Result)) error {
				return client.RunHeartbeat(ctx, func() controlplane.State { return runner.HeartbeatState(readiness.get()) }, report)
			}, runner.Resume, report, errorOutput)
		},
		backups: func(ctx context.Context) error {
			return runScheduledBackups(ctx, path, scheduler, runner, client, startupReady, output, errorOutput)
		},
		restores: restores.Run,
		setup: func(ctx context.Context) error {
			return runDashboardSetup(ctx, path, client, setupPollInterval, func(ctx context.Context) {
				finishDashboardSetup(ctx, path, client, readiness, output, errorOutput)
			}, errorOutput)
		},
		update: func(ctx context.Context) (bool, error) {
			effective, err := config.LoadAgent(path)
			if err != nil || !effective.AutoUpdate {
				return false, err
			}

			return checkAndMaybeInstall(ctx, path, effective, true, true)
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
	backups    func(context.Context) error
	restores   func(context.Context) error
	setup      func(context.Context) error
	update     func(context.Context) (bool, error)
}

// heartbeatReadiness starts from the startup diagnostics and is refreshed when
// dashboard setup completes, so a finished setup is reported without a restart.
type heartbeatReadiness struct {
	mu    sync.Mutex
	state controlplane.State
}

func (r *heartbeatReadiness) set(report diagnostic.Report) {
	state := startupHeartbeatState(report.Checks)
	if report.Driver != "" && report.Version != "" {
		state.DatabaseMetadata = controlplane.DatabaseMetadata{Driver: report.Driver, Version: report.Version}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = state
}

func (r *heartbeatReadiness) get() controlplane.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
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
	backupDone := make(chan error, 1)
	restoreDone := make(chan error, 1)
	setupDone := make(chan error, 1)
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
	backupsStarted := false
	restoresStarted := false
	setupStarted := false
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
			if !backupsStarted && work.backups != nil && (result.Connection == controlplane.Connected || result.Connection == controlplane.Unreachable) {
				backupsStarted = true
				workers.Add(1)
				go func() {
					defer workers.Done()
					backupDone <- work.backups(ctx)
				}()
			}
			if !restoresStarted && work.restores != nil && (result.Connection == controlplane.Connected || result.Connection == controlplane.Unreachable) {
				restoresStarted = true
				workers.Add(1)
				go func() { defer workers.Done(); restoreDone <- work.restores(ctx) }()
			}
			if !setupStarted && work.setup != nil && (result.Connection == controlplane.Connected || result.Connection == controlplane.Unreachable) {
				setupStarted = true
				workers.Add(1)
				go func() { defer workers.Done(); setupDone <- work.setup(ctx) }()
			}
			if autoUpdate && !updaterStarted && (result.Connection == controlplane.Connected || result.Connection == controlplane.Unreachable) {
				updaterStarted = true
				workers.Add(1)
				go func() {
					defer workers.Done()
					runAgentUpdates(ctx, work.update, updates)
				}()
			}
		case err := <-backupDone:
			return err
		case err := <-restoreDone:
			if errors.Is(err, controlplane.ErrCredentialRejected) {
				return err
			}
			if ctx.Err() == nil {
				if err != nil {
					fmt.Fprintf(errorOutput, "Restore delivery stopped: %v\n", err)
				}
				fmt.Fprintln(errorOutput, "Restore delivery is unavailable; backup and heartbeat workers continue. Check local restore access/state and restart the service after resolving it.")
			}
		case err := <-setupDone:
			if errors.Is(err, controlplane.ErrCredentialRejected) {
				return err
			}
			if err != nil && ctx.Err() == nil {
				fmt.Fprintf(errorOutput, "Dashboard setup stopped: %v\n", err)
				fmt.Fprintln(errorOutput, "Backup and heartbeat workers continue. Finish setup with db-agent configure --key, or restart the service to retry dashboard setup.")
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
