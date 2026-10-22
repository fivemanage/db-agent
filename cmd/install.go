package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/diagnostic"
	"github.com/fivemanage/db-agent/internal/enrollment"
	services "github.com/fivemanage/db-agent/internal/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Release builds supply the production endpoint; development may override it.
var defaultAPIURL string
var installAPIURL string
var installServiceOnly bool

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Register or resume setup and install the agent service",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := config.ResolvePath(configPath)
		if installServiceOnly {
			if _, err := config.LoadAgent(path); err != nil {
				return fmt.Errorf("service installation needs a saved enrollment: %w", err)
			}

			return controlService(path, cmd.OutOrStdout(), services.NewController, "install", "start")
		}

		output := newSetupOutput(cmd.OutOrStdout())
		output.title("FiveManage / Database backup setup", "Enrollment -> Database -> Recovery key -> Checks -> Service")
		output.status("INFO", "Have your dashboard setup token and local database connection ready.")
		output.status("WARN", "Turn off terminal recording and PowerShell transcripts before continuing.")
		output.step(1, 5, "Connect to FiveManage", "New installs use a one-time setup token. Existing enrollment is reused.")

		settings, err := ensureEnrollment(cmd.Context(), path, installAPIURL, func() (string, error) {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return "", errors.New("install requires an interactive terminal for hidden token input")
			}

			fmt.Fprint(cmd.ErrOrStderr(), "Setup token (hidden): ")
			token, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(cmd.ErrOrStderr())
			if err != nil {
				return "", errors.New("cannot read setup token from terminal")
			}

			return strings.TrimSpace(string(token)), nil
		}, func(ctx context.Context, apiURL, token, version string) (string, error) {
			return enrollment.Enroll(ctx, path, apiURL, token, version)
		})
		if err != nil {
			output.status("FAIL", "Enrollment could not be completed. Database and recovery-key setup have not started.")
			return err
		}

		output.status("OK", "Enrollment saved. Resuming setup does not require another token.")
		output.detail("Config", fmt.Sprintf("%q", path))
		output.step(2, 5, "Connect your database", "Set up read-only backups and optionally enable dashboard restores. Credentials stay on this machine.")
		if settings.Database.Validate() == nil && len(settings.Database.Databases) > 0 {
			output.status("OK", "Existing database settings preserved; they will be checked in step 4.")
			output.detail("Change", "db-agent configure")
			if err := configureInstallRestoreAccess(cmd, bufio.NewReader(cmd.InOrStdin()), config.DatabaseConfig{}); err != nil {
				return err
			}
		} else if err := configureLocalDatabase(cmd, settings.Database, true); err != nil {
			return err
		}

		output.step(3, 5, "Save your recovery key", "Keep an offline copy. FiveManage cannot restore your backups without it.")
		if err := configureRecoveryKey(cmd, path); err != nil {
			output.status("FAIL", "Recovery-key setup is incomplete. Any existing key is preserved.")
			output.detail("Resume", "db-agent configure --key")
			return err
		}

		output.step(4, 5, "Check backup prerequisites", "Checking the database, dump tools, recovery key, and API reachability...")
		settings, err = config.LoadDiagnostic(path)
		if err != nil {
			return fmt.Errorf("database settings were saved, but prerequisites could not be checked: %w; retry with db-agent test", err)
		}
		if !printChecks(cmd.OutOrStdout(), diagnostic.Run(cmd.Context(), settings)) {
			output.status("FAIL", "Prerequisites need attention. The service was not installed or started.")
			output.status("INFO", "Enrollment and database settings remain saved; no existing key was changed.")
			output.detail("Next", "Correct the failed checks, then rerun db-agent install.")
			return errors.New("setup incomplete: prerequisites failed; service was not installed or started; no working backup exists yet")
		}
		output.status("OK", "All backup prerequisites passed. These checks do not execute a backup.")

		output.step(5, 5, "Start the background service", "Installing and starting the service with your saved configuration...")
		if err := controlService(path, cmd.OutOrStdout(), services.NewController, "install", "start"); err != nil {
			output.status("FAIL", "Service setup did not finish. Enrollment, database settings, and the recovery key remain saved.")
			return err
		}

		output.section("Setup complete")
		output.status("OK", "Service started. Backups run in the background.")
		output.status("INFO", "A running service is not proof of a completed backup.")
		output.detail("Next", "db-agent status  (check the first backup outcome)")
		output.detail("Diagnose", "db-agent test")
		return nil
	},
}

func ensureEnrollment(ctx context.Context, path, apiURL string, readToken func() (string, error), register func(context.Context, string, string, string) (string, error)) (config.Config, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return config.Config{}, errors.New("existing config must be a regular file; setup will not replace it")
		}

		settings, err := config.LoadDiagnostic(path)
		if err != nil {
			return config.Config{}, err
		}

		if err := settings.ValidateAgent(); err != nil {
			return config.Config{}, fmt.Errorf("existing enrollment is incomplete; setup will not overwrite it: %w", err)
		}

		if apiURL != "" && apiURL != settings.APIURL {
			return config.Config{}, errors.New("existing config uses a different API URL; setup will not re-enroll or replace credentials")
		}

		return settings, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return config.Config{}, fmt.Errorf("check config path: %w", err)
	}

	if apiURL == "" {
		pending, err := config.LoadPendingEnrollment(path)
		if err == nil {
			apiURL = pending.APIURL
		} else if !errors.Is(err, os.ErrNotExist) {
			return config.Config{}, err
		} else {
			apiURL = defaultAPIURL
		}
	}

	if apiURL == "" {
		return config.Config{}, errors.New("this development build has no production API URL configured; use --api-url for development, or install a release build")
	}

	if err := enrollment.ValidateAPIURL(apiURL); err != nil {
		return config.Config{}, err
	}

	if recovered, err := enrollment.Recover(ctx, path, apiURL, version); err != nil {
		return config.Config{}, err
	} else if recovered {
		return config.LoadDiagnostic(path)
	}

	token, err := readToken()
	if err != nil {
		return config.Config{}, err
	}

	credential, err := register(ctx, apiURL, token, version)
	if err != nil {
		return config.Config{}, fmt.Errorf("register agent: %w", err)
	}

	if err := config.SaveEnrollment(path, apiURL, credential); err != nil {
		if current, loadErr := config.LoadDiagnostic(path); loadErr == nil && current.AgentCredential == credential && current.APIURL == apiURL {
			return current, nil
		}

		return config.Config{}, fmt.Errorf("registration succeeded, but saving the agent credential failed: %w; contact support before generating another setup token", err)
	}

	return config.LoadDiagnostic(path)
}

func init() {
	installCmd.Flags().StringVar(&installAPIURL, "api-url", "", "override control plane URL for development")
	installCmd.Flags().BoolVar(&installServiceOnly, "service-only", false, "install/start using saved enrollment, without running setup or claiming backup readiness")
	rootCmd.AddCommand(installCmd)
}
