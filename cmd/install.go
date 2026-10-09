package cmd

import (
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
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Enrollment saved at %s. Resuming setup does not require another token.\n", path)
		if settings.Database.Validate() == nil && len(settings.Database.Databases) > 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "Existing database settings preserved. Use db-agent configure to explicitly change them, or db-agent test to diagnose them.")
		} else if err := configureCmd.RunE(cmd, nil); err != nil {
			return err
		}

		if err := configureRecoveryKey(cmd, path); err != nil {
			return err
		}

		settings, err = config.LoadDiagnostic(path)
		if err != nil {
			return fmt.Errorf("database settings were saved, but prerequisites could not be checked: %w; retry with db-agent test", err)
		}
		if !printChecks(cmd.OutOrStdout(), diagnostic.Run(cmd.Context(), settings)) {
			fmt.Fprintln(cmd.OutOrStdout(), "Correct the failed prerequisites and rerun db-agent test. Enrollment and database settings remain saved; no existing key was changed.")
			return errors.New("setup incomplete: prerequisites failed; service was not installed or started; no working backup exists yet")
		}

		if err := controlService(path, cmd.OutOrStdout(), services.NewController, "install", "start"); err != nil {
			return err
		}

		fmt.Fprintln(cmd.OutOrStdout(), "Service started. Backups run in the background; use db-agent status to check the first outcome.")
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
