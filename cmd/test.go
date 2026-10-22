package cmd

import (
	"errors"
	"fmt"
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
		settings, err := config.LoadDiagnostic(configPath)
		if err != nil {
			printChecks(cmd.OutOrStdout(), []database.Check{{Name: "Config", Message: err.Error()}})
			return errors.New("diagnostics failed; fix the config and rerun db-agent test")
		}

		checks := diagnostic.Run(cmd.Context(), settings)
		if !printChecks(cmd.OutOrStdout(), checks) {
			return errors.New("diagnostics failed; address the failed checks and rerun db-agent test")
		}

		fmt.Fprintln(cmd.OutOrStdout(), "All diagnostic checks passed. No backup was executed; agent authentication and key format are not verified.")

		return nil
	},
}

func printChecks(output io.Writer, checks []database.Check) bool {
	view := newSetupOutput(output)
	passed := true
	for _, check := range checks {
		status := "PASS"
		if !check.Passed {
			passed = false
			status = "FAIL"
		}
		if check.Skipped {
			status = "SKIP"
		}

		fmt.Fprintf(output, "%s %s: %s\n", view.badge(status), check.Name, check.Message)
	}

	return passed
}

func init() {
	rootCmd.AddCommand(testCmd)
}
