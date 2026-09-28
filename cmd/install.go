package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/enrollment"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var installAPIURL string

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Exchange a setup token and save the agent credential",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if installAPIURL == "" {
			return errors.New("--api-url is required until the control plane URL is configured by the installer")
		}
		if err := enrollment.ValidateAPIURL(installAPIURL); err != nil {
			return err
		}
		path := config.ResolvePath(configPath)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("config already exists at %s; enrollment will not overwrite it", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check config path: %w", err)
		}

		fmt.Fprint(cmd.ErrOrStderr(), "Setup token: ")
		token, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return fmt.Errorf("read setup token: %w", err)
		}

		setupToken := strings.TrimSpace(string(token))
		if setupToken == "" {
			return errors.New("setup token is required")
		}

		credential, err := enrollment.Register(cmd.Context(), installAPIURL, setupToken, version)
		if err != nil {
			return fmt.Errorf("register agent: %w", err)
		}
		if err := config.SaveEnrollment(path, installAPIURL, credential); err != nil {
			return fmt.Errorf("registration succeeded, but saving the agent credential failed: %w; contact support before generating another setup token", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Agent registered. Credential saved to %s. Service installation is not implemented yet.\n", path)
		return nil
	},
}

func init() {
	installCmd.Flags().StringVar(&installAPIURL, "api-url", "", "control plane base URL")
	rootCmd.AddCommand(installCmd)
}
