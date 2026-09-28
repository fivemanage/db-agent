package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/update"
	"github.com/spf13/cobra"
)

// Set by release builds using -ldflags. An absent key disables updates rather
// than trusting a key supplied by the manifest endpoint.
var updatePublicKey string

func newUpdateClient(settings config.Config) (update.Client, error) {
	key, err := update.PublicKey(updatePublicKey)
	if err != nil {
		return update.Client{}, err
	}
	manifestURL := settings.UpdateURL
	if manifestURL == "" {
		manifestURL = strings.TrimRight(settings.APIURL, "/") + "/v1/agent/updates"
	}
	manifestURL = strings.ReplaceAll(strings.ReplaceAll(manifestURL, "{os}", runtime.GOOS), "{arch}", runtime.GOARCH)
	return update.Client{
		PublicKey:   key,
		Version:     version,
		ManifestURL: manifestURL,
	}, nil
}

func checkAndMaybeInstall(ctx context.Context, settings config.Config, install, restartAgent bool) (bool, error) {
	client, err := newUpdateClient(settings)
	if err != nil {
		return false, err
	}
	manifest, available, err := client.Check(ctx)
	if err != nil || !available || !install {
		return available, err
	}
	executablePath, err := os.Executable()
	if err != nil {
		return false, err
	}
	executablePath, err = filepath.EvalSymlinks(executablePath)
	if err != nil {
		return false, err
	}
	stagedPath, err := client.Stage(ctx, manifest, executablePath)
	if err != nil {
		return false, err
	}
	serviceName := os.Getenv("DB_AGENT_SERVICE_NAME")
	if err := update.LaunchHelper(executablePath, stagedPath, manifest.SHA256, manifest.Version, update.Restart{
		ServiceName: serviceName,
		ConfigPath:  config.ResolvePath(configPath),
		NoRestart:   !restartAgent && serviceName == "",
	}); err != nil {
		os.Remove(stagedPath)
		return false, err
	}
	return true, nil
}

var updateCmd = &cobra.Command{Use: "update", Short: "Check for or install a signed agent update"}

var updateCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check the signed update feed",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		settings, err := config.LoadUpdate(configPath)
		if err != nil {
			return err
		}
		available, err := checkAndMaybeInstall(cmd.Context(), settings, false, false)
		if err != nil {
			return err
		}
		if available {
			fmt.Fprintln(cmd.OutOrStdout(), "A signed update is available.")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "Agent is up to date.")
		}
		return nil
	},
}

var updateNowCmd = &cobra.Command{
	Use:   "now",
	Short: "Download and install a signed agent update",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		settings, err := config.LoadUpdate(configPath)
		if err != nil {
			return err
		}
		available, err := checkAndMaybeInstall(cmd.Context(), settings, true, false)
		if err != nil {
			return err
		}
		if available {
			fmt.Fprintln(cmd.OutOrStdout(), "Update verified; replacement helper started. Check the installed version after it exits.")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "Agent is up to date.")
		}
		return nil
	},
}

var helperTarget, helperStaged, helperSHA, helperVersion, helperService, helperConfig string
var helperNoRestart bool

var updateHelperCmd = &cobra.Command{
	Use:    "update-helper",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if err := update.RunHelper(helperTarget, helperStaged, helperSHA, helperVersion, update.Restart{ServiceName: helperService, ConfigPath: helperConfig, NoRestart: helperNoRestart}); err != nil {
			return err
		}
		return nil
	},
}

func init() {
	updateHelperCmd.Flags().StringVar(&helperTarget, "target", "", "installed executable")
	updateHelperCmd.Flags().StringVar(&helperStaged, "staged", "", "verified update")
	updateHelperCmd.Flags().StringVar(&helperSHA, "sha256", "", "expected SHA-256")
	updateHelperCmd.Flags().StringVar(&helperVersion, "expected-version", "", "expected agent version")
	updateHelperCmd.Flags().StringVar(&helperService, "service", "", "service to restart")
	updateHelperCmd.Flags().StringVar(&helperConfig, "restart-config", "", "config path for a foreground restart")
	updateHelperCmd.Flags().BoolVar(&helperNoRestart, "no-restart", false, "do not restart after manual replacement")
	updateCmd.AddCommand(updateCheckCmd, updateNowCmd)
	rootCmd.AddCommand(updateCmd, updateHelperCmd)
}
