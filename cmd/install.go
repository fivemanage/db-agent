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
		output.title("FiveManage database backups", "Connect this server in a few quick steps. Database logins stay on this machine.")
		output.warn("Turn off terminal recording first. Your recovery key is shown once.")
		output.step(1, 5, "Connect to FiveManage")

		settings, err := ensureEnrollment(cmd.Context(), path, installAPIURL, func() (string, error) {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return "", errors.New("install requires an interactive terminal for hidden token input")
			}

			newSetupOutput(cmd.ErrOrStderr()).ask("Setup token from your dashboard", "hidden")
			token, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(cmd.ErrOrStderr())
			if err != nil {
				return "", errors.New("cannot read setup token from terminal")
			}

			return strings.TrimSpace(string(token)), nil
		}, func(ctx context.Context, apiURL, token, version string) (string, error) {
			stop := output.spin("Registering this server")
			defer stop()
			return enrollment.Enroll(ctx, path, apiURL, token, version)
		})
		if err != nil {
			output.fail("Couldn't connect to FiveManage.")
			return err
		}

		output.ok("Connected to FiveManage")
		output.step(2, 5, "Connect your database")
		if settings.Database.Validate() == nil && len(settings.Database.Databases) > 0 {
			output.ok("Using your saved database settings")
			output.note("To change them, run db-agent configure")
			if err := configureInstallRestoreAccess(cmd, bufio.NewReader(cmd.InOrStdin()), config.DatabaseConfig{}); err != nil {
				return err
			}
		} else if err := configureLocalDatabase(cmd, settings.Database, true); err != nil {
			return err
		}

		output.step(3, 5, "Save your recovery key")
		if err := configureRecoveryKey(cmd, path); err != nil {
			output.fail("Recovery key setup isn't finished. Your existing key is kept.")
			output.note("Resume with db-agent configure --key")
			return err
		}

		output.step(4, 5, "Check everything")
		settings, err = config.LoadDiagnostic(path)
		if err != nil {
			return fmt.Errorf("database settings were saved, but prerequisites could not be checked: %w; retry with db-agent test", err)
		}
		stop := output.spin("Checking database, backup tools, recovery key, and FiveManage")
		checks := diagnostic.Run(cmd.Context(), settings)
		stop()
		if !checksPassed(checks) {
			printFailedChecks(cmd.OutOrStdout(), checks)
			output.fail("Some checks failed, so the backup service wasn't started.")
			output.note("Your progress is saved. Fix the issues above, then run db-agent install again.")
			return errors.New("setup incomplete: prerequisites failed; service was not installed or started; no working backup exists yet")
		}
		output.ok(fmt.Sprintf("All %d checks passed", len(checks)))

		output.step(5, 5, "Start the backup service")
		stop = output.spin("Installing and starting the service")
		messages, err := serviceActions(path, services.NewController, "install", "start")
		stop()
		for _, message := range messages {
			output.ok(message)
		}
		if err != nil {
			output.fail("The service didn't start. Your settings and recovery key are saved.")
			return err
		}

		output.section("You're all set")
		output.info("Your first backup starts in the background.")
		output.note("Check it in the dashboard or with db-agent status")
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
