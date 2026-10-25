package cmd

import (
	"errors"
	"io"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/diagnostic"
	"github.com/spf13/cobra"
)

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Diagnose local database and backup prerequisites",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		view := newSetupOutput(cmd.OutOrStdout())
		settings, err := config.LoadDiagnostic(configPath)
		if err != nil {
			printChecks(cmd.OutOrStdout(), []database.Check{{Name: "Config", Message: err.Error()}})
			return errors.New("diagnostics failed; fix the config and rerun db-agent test")
		}

		stop := view.spin("Checking your setup")
		checks := diagnostic.Run(cmd.Context(), settings)
		stop()
		if !printChecks(cmd.OutOrStdout(), checks) {
			return errors.New("diagnostics failed; address the failed checks and rerun db-agent test")
		}

		view.ok("All checks passed.")
		view.note("This doesn't run a backup. Use db-agent backup --now to try one.")

		return nil
	},
}

func printChecks(output io.Writer, checks []database.Check) bool {
	view := newSetupOutput(output)
	for _, check := range checks {
		view.check(check)
	}

	return checksPassed(checks)
}

// printFailedChecks shows only actionable failures. Skipped checks are
// consequences of an earlier failure and would only add noise.
func printFailedChecks(output io.Writer, checks []database.Check) {
	view := newSetupOutput(output)
	for _, check := range checks {
		if !check.Passed && !check.Skipped {
			view.check(check)
		}
	}
}

func checksPassed(checks []database.Check) bool {
	for _, check := range checks {
		if !check.Passed {
			return false
		}
	}

	return true
}

func init() {
	rootCmd.AddCommand(testCmd)
}
