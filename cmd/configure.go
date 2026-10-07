package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var configureKeyOnly bool

var configureCmd = &cobra.Command{
	Use:   "configure",
	Short: "Connect a registered agent to explicitly selected local databases",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, err := config.LoadDiagnostic(configPath)
		if err != nil {
			return err
		}

		if err := settings.ValidateAgent(); err != nil {
			return err
		}

		if configureKeyOnly {
			return configureRecoveryKey(cmd, config.ResolvePath(configPath))
		}

		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("configure requires an interactive terminal for hidden password input; alternatively edit database settings in the private config file")
		}

		return configureDatabase(cmd.Context(), bufio.NewReader(cmd.InOrStdin()), cmd.OutOrStdout(), settings.Database, func() (string, error) {
			password, err := term.ReadPassword(int(os.Stdin.Fd()))
			return string(password), err
		}, databaseSetup{
			inspect:   database.Inspect,
			provision: database.ProvisionBackupAccount,
			save:      func(candidate config.DatabaseConfig) error { return config.SaveDatabase(configPath, candidate) },
		})
	},
}

type databaseSetup struct {
	inspect   func(context.Context, config.DatabaseConfig) database.Report
	provision func(context.Context, config.DatabaseConfig, string, string, func(config.DatabaseConfig) error) error
	save      func(config.DatabaseConfig) error
}

func configureDatabase(ctx context.Context, input *bufio.Reader, output io.Writer, previous config.DatabaseConfig, readSecret func() (string, error), setup databaseSetup) error {
	fmt.Fprintln(output, "Connect the database on this machine. Supplied credentials stay in memory until a tested backup account is confirmed; admin credentials are never saved.")

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		candidate, err := promptDatabaseConnection(input, output, previous, readSecret)
		if err == nil {
			err = setupDatabase(ctx, input, output, candidate, setup)
		}

		if ctx.Err() != nil {
			return fmt.Errorf("setup canceled: %w; enrollment and existing config/key are preserved", ctx.Err())
		}
		if err == nil {
			fmt.Fprintln(output, "Tested backup account saved privately. Run db-agent test to check remaining prerequisites. No service was installed and no backup was executed.")
			return nil
		}

		fmt.Fprintf(output, "Database setup: %v\n", err)
		retry, inputErr := prompt(input, output, "Try another connection or existing backup account? [y/N]", "N")
		if inputErr != nil || !affirmative(retry) {
			return fmt.Errorf("%w; enrollment and existing config/key are preserved; resume with db-agent configure", err)
		}
	}
}

func setupDatabase(ctx context.Context, input *bufio.Reader, output io.Writer, candidate config.DatabaseConfig, setup databaseSetup) error {
	report := setup.inspect(ctx, candidate)
	printChecks(output, report.Checks)

	// Backup grants may be missing on an admin account, but connectivity,
	// version and the selected schema must pass before any SQL changes.
	if len(report.Checks) < 4 {
		return errors.New("database connection checks failed; no settings were changed")
	}
	for _, check := range report.Checks[:4] {
		if !check.Passed {
			return errors.New("database connection or selection failed; no settings were changed")
		}
	}

	candidate.Driver = report.Driver
	statements, planErr := database.LocalBackupAccountSQL(report.Driver, report.Version, candidate.Databases, candidate.Host)
	if planErr == nil {
		fmt.Fprintln(output, "Least-privilege account SQL (password placeholder is replaced privately; existing accounts are never modified):")
		for _, statement := range statements {
			fmt.Fprintf(output, "%s;\n", statement)
		}
	} else {
		fmt.Fprintf(output, "Automatic account creation unavailable: %v\n", planErr)
	}

	if report.CanCreateUser && planErr == nil {
		create, err := prompt(input, output, "Execute this SQL using the supplied account? [y/N]", "N")
		if err != nil {
			return err
		}
		if affirmative(create) {
			return setup.provision(ctx, candidate, report.Driver, report.Version, func(backup config.DatabaseConfig) error {
				verified := setup.inspect(ctx, backup)
				printChecks(output, verified.Checks)
				if !verified.Passed() || verified.Administrative {
					return errors.New("generated account could not be verified; no database settings were saved")
				}

				if err := ctx.Err(); err != nil {
					return err
				}

				return setup.save(backup)
			})
		}
	}

	if report.Administrative || strings.EqualFold(candidate.User, "root") {
		return errors.New("admin credentials will not be saved; supply an existing backup account, or retry and approve scoped account creation")
	}

	if !report.Passed() {
		return errors.New("this account lacks verified backup grants; ask a local administrator to grant the displayed schema/routine privileges to your backup account, then retry")
	}

	confirm, err := prompt(input, output, "Use this existing backup account and save credentials locally for the confirmed scope? [y/N]", "N")
	if err != nil {
		return err
	}
	if !affirmative(confirm) {
		return errors.New("configuration canceled; no settings were changed")
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	return setup.save(candidate)
}

func promptDatabaseConnection(input *bufio.Reader, output io.Writer, previous config.DatabaseConfig, readSecret func() (string, error)) (config.DatabaseConfig, error) {
	mode, err := prompt(input, output, "Connection input: 1 = paste game-server connection string, 2 = enter manually", "1")
	if err != nil {
		return config.DatabaseConfig{}, err
	}

	var candidate config.DatabaseConfig
	switch mode {
	case "1":
		fmt.Fprint(output, "Connection string (hidden; paste only its value): ")
		secret, readErr := readSecret()
		fmt.Fprintln(output)
		if readErr != nil {
			return config.DatabaseConfig{}, errors.New("cannot read connection string from terminal")
		}
		candidate, err = database.ParseConnection(secret)
	case "2":
		candidate, err = promptConnection(input, output, previous, readSecret)
		if err == nil {
			fallback := ""
			if len(previous.Databases) == 1 {
				fallback = previous.Databases[0]
			}
			var name string
			name, err = prompt(input, output, "Application database name", fallback)
			candidate.Databases = []string{name}
		}
	default:
		err = errors.New("select connection input 1 or 2")
	}

	if err != nil {
		return config.DatabaseConfig{}, err
	}
	if err := database.ValidateLocalConnection(candidate); err != nil {
		return config.DatabaseConfig{}, err
	}

	fmt.Fprintf(output, "Local connection: host=%q port=%d user=%q databases=%q (password hidden)\n", candidate.Host, candidate.Port, candidate.User, candidate.Databases)
	confirm, err := prompt(input, output, "Test this connection and backup scope? [y/N]", "N")
	if err != nil {
		return config.DatabaseConfig{}, err
	}
	if !affirmative(confirm) {
		return config.DatabaseConfig{}, errors.New("connection not confirmed; no settings were changed")
	}

	return candidate, nil
}

func affirmative(value string) bool {
	return strings.EqualFold(value, "y") || strings.EqualFold(value, "yes")
}

func promptConnection(input *bufio.Reader, output io.Writer, candidate config.DatabaseConfig, readSecret func() (string, error)) (config.DatabaseConfig, error) {
	var err error
	if candidate.Host == "" {
		candidate.Host = "127.0.0.1"
	}
	if candidate.Port == 0 {
		candidate.Port = 3306
	}
	if candidate.Driver == "" {
		candidate.Driver = "mysql"
	}

	candidate.Host, err = prompt(input, output, "Database host", candidate.Host)
	if err != nil {
		return config.DatabaseConfig{}, err
	}

	port, err := prompt(input, output, "Database TCP port", strconv.Itoa(candidate.Port))
	if err != nil {
		return config.DatabaseConfig{}, err
	}
	candidate.Port, err = strconv.Atoi(port)
	if err != nil {
		return config.DatabaseConfig{}, errors.New("database port must be a number between 1 and 65535")
	}

	candidate.User, err = prompt(input, output, "Local account username (existing game-server or backup account)", candidate.User)
	if err != nil {
		return config.DatabaseConfig{}, err
	}

	fmt.Fprint(output, "Database password (hidden): ")
	candidate.Password, err = readSecret()
	fmt.Fprintln(output)
	if err != nil {
		return config.DatabaseConfig{}, errors.New("cannot read database password from terminal")
	}

	candidate.Databases = nil

	return candidate, nil
}

func promptScope(input *bufio.Reader, output io.Writer, visible []string) ([]string, error) {
	fmt.Fprintln(output, "Visible application databases:")
	for i, name := range visible {
		fmt.Fprintf(output, "  %d. %q\n", i+1, name)
	}

	choice, err := prompt(input, output, "Select database numbers (comma-separated; no default)", "")
	if err != nil {
		return nil, err
	}

	selected, err := selectDatabases(choice, visible)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(output, "Backup scope: %q\n", selected)
	confirm, err := prompt(input, output, "Save credentials locally for this scope? [y/N]", "N")
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(confirm, "y") && !strings.EqualFold(confirm, "yes") {
		return nil, errors.New("configuration canceled; no settings were changed")
	}

	return selected, nil
}

func prompt(input *bufio.Reader, output io.Writer, label, fallback string) (string, error) {
	if fallback == "" {
		fmt.Fprintf(output, "%s: ", label)
	} else {
		fmt.Fprintf(output, "%s [%s]: ", label, strconv.Quote(fallback))
	}

	line, err := input.ReadString('\n')
	if err != nil {
		return "", errors.New("setup input ended; no settings were changed")
	}

	line = strings.TrimSpace(line)
	if line == "" {
		line = fallback
	}

	return line, nil
}

func selectDatabases(choice string, visible []string) ([]string, error) {
	var selected []string
	seen := map[int]bool{}
	for _, part := range strings.Split(choice, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > len(visible) {
			return nil, errors.New("select at least one valid database number; no settings were changed")
		}

		if !seen[n] {
			selected = append(selected, visible[n-1])
			seen[n] = true
		}
	}

	return selected, nil
}

func init() {
	configureCmd.Flags().BoolVar(&configureKeyOnly, "key", false, "set up or acknowledge the local recovery key without changing database settings")
	rootCmd.AddCommand(configureCmd)
}
