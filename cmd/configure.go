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
	"unicode"
	"unicode/utf8"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var configureKeyOnly bool
var configureRestoreAccess bool

var configureCmd = &cobra.Command{
	Use:   "configure",
	Short: "Connect a registered agent to explicitly selected local databases",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, err := config.LoadDiagnostic(configPath)
		if err != nil {
			return err
		}
		if configureRestoreAccess {
			newSetupOutput(cmd.OutOrStdout()).title("Restore access", "Use an existing restore user for dashboard restores.")
			return configureLocalRestoreAccess(cmd, settings)
		}

		if err := settings.ValidateAgent(); err != nil {
			return err
		}

		if configureKeyOnly {
			newSetupOutput(cmd.OutOrStdout()).title("Recovery key", "Confirm you have a copy of your recovery key.")
			newSetupOutput(cmd.OutOrStdout()).warn("Turn off terminal recording first. A new recovery key is shown once.")
			return configureRecoveryKey(cmd, config.ResolvePath(configPath))
		}

		newSetupOutput(cmd.OutOrStdout()).title("Database connection", "Your database login stays on this machine.")
		return configureLocalDatabase(cmd, settings.Database, false)
	},
}

func configureLocalDatabase(cmd *cobra.Command, previous config.DatabaseConfig, dashboardRestores bool) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("configure requires an interactive terminal for hidden password input; alternatively edit database settings in the private config file")
	}

	input := bufio.NewReader(cmd.InOrStdin())
	setup := databaseSetup{
		inspect:   database.Inspect,
		provision: database.ProvisionBackupAccount,
		save:      func(candidate config.DatabaseConfig) error { return config.SaveDatabase(configPath, candidate) },
	}
	if dashboardRestores {
		setup.restore = func(admin config.DatabaseConfig) error {
			return configureInstallRestoreAccess(cmd, input, admin)
		}
	}
	return configureDatabase(cmd.Context(), input, cmd.OutOrStdout(), previous, func() (string, error) {
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(password), err
	}, setup)
}

type databaseSetup struct {
	inspect   func(context.Context, config.DatabaseConfig) database.Report
	provision func(context.Context, config.DatabaseConfig, string, string, string, func(config.DatabaseConfig) error) error
	save      func(config.DatabaseConfig) error
	restore   func(config.DatabaseConfig) error
}

func configureDatabase(ctx context.Context, input *bufio.Reader, output io.Writer, previous config.DatabaseConfig, readSecret func() (string, error), setup databaseSetup) error {
	view := newSetupOutput(output)

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
			if setup.restore != nil {
				return setup.restore(candidate)
			}
			view.note("Run db-agent test to check the full setup.")
			return nil
		}

		view.fail(sentence(err.Error()))
		retry, inputErr := confirm(input, output, "Try again?", true)
		if inputErr != nil || !retry {
			return fmt.Errorf("%w; enrollment and existing config/key are preserved; resume with db-agent configure", err)
		}
	}
}

func setupDatabase(ctx context.Context, input *bufio.Reader, output io.Writer, candidate config.DatabaseConfig, setup databaseSetup) error {
	view := newSetupOutput(output)
	stop := view.spin("Testing the connection")
	report := setup.inspect(ctx, candidate)
	stop()

	// Backup grants may be missing on an admin account, but connectivity,
	// version and the selected schema must pass before any SQL changes.
	if len(report.Checks) < 4 || !checksPassed(report.Checks[:4]) {
		printFailedChecks(output, report.Checks)
		return fmt.Errorf("couldn't connect to %s; nothing was changed", describeConnection(candidate))
	}
	view.ok(fmt.Sprintf("Connected to %s %s (%s)", serverName(report.Driver), displayText(report.Version), describeConnection(candidate)))

	candidate.Driver = report.Driver
	if report.CanCreateUser {
		_, planErr := database.LocalBackupAccountSQL(report.Driver, report.Version, candidate.Databases, candidate.Host, report.ClientHost)
		if planErr != nil {
			view.warn(fmt.Sprintf("Can't create a backup user automatically: %v", planErr))
		} else {
			view.note("This is an admin login. We'll use it once to create a read-only backup user and never save it.")
			create, err := confirm(input, output, "Create a read-only backup user?", true)
			if err != nil {
				return err
			}
			if create {
				return createBackupAccount(ctx, output, candidate, report, setup)
			}
		}
	}

	if report.Administrative || strings.EqualFold(candidate.User, "root") {
		return errors.New("admin logins are never saved; let setup create a backup user, or connect with an existing backup user")
	}

	if !report.Passed() {
		printFailedChecks(output, report.Checks)
		return errors.New("this login is missing backup permissions; ask your database administrator to grant them, then try again")
	}

	use, err := confirm(input, output, "Use this login for backups? It's saved on this machine only.", false)
	if err != nil {
		return err
	}
	if !use {
		return errors.New("configuration canceled; no settings were changed")
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := setup.save(candidate); err != nil {
		return err
	}

	view.ok("Backup login saved")
	return nil
}

func createBackupAccount(ctx context.Context, output io.Writer, admin config.DatabaseConfig, report database.Report, setup databaseSetup) error {
	view := newSetupOutput(output)
	var failed []database.Check
	stop := view.spin("Creating the backup user")
	err := setup.provision(ctx, admin, report.Driver, report.Version, report.ClientHost, func(backup config.DatabaseConfig) error {
		verified := setup.inspect(ctx, backup)
		if !verified.Passed() || verified.Administrative {
			failed = verified.Checks
			return errors.New("generated account could not be verified; no database settings were saved")
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		return setup.save(backup)
	})
	stop()
	printFailedChecks(output, failed)
	if err != nil {
		return err
	}

	view.ok("Read-only backup user created")
	return nil
}

func describeConnection(candidate config.DatabaseConfig) string {
	names := make([]string, len(candidate.Databases))
	for i, name := range candidate.Databases {
		names[i] = displayText(name)
	}

	return fmt.Sprintf("%s@%s:%d/%s", displayText(candidate.User), displayText(candidate.Host), candidate.Port, strings.Join(names, ","))
}

func sentence(message string) string {
	first, size := utf8.DecodeRuneInString(message)
	if size == 0 {
		return message
	}

	return string(unicode.ToUpper(first)) + message[size:]
}

func serverName(driver string) string {
	switch driver {
	case "mariadb":
		return "MariaDB"
	case "mysql":
		return "MySQL"
	default:
		return displayText(driver)
	}
}

func promptDatabaseConnection(input *bufio.Reader, output io.Writer, previous config.DatabaseConfig, readSecret func() (string, error)) (config.DatabaseConfig, error) {
	view := newSetupOutput(output)
	view.info("How should we connect to your database?")
	view.note("1  Paste your game server's connection string (recommended)")
	view.note("2  Enter the details manually")
	mode, err := prompt(input, output, "Choose", "1")
	if err != nil {
		return config.DatabaseConfig{}, err
	}

	var candidate config.DatabaseConfig
	switch mode {
	case "1":
		view.ask("Connection string", "hidden")
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
			name, err = prompt(input, output, "Database name", fallback)
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

	candidate.Host, err = prompt(input, output, "Host", candidate.Host)
	if err != nil {
		return config.DatabaseConfig{}, err
	}

	port, err := prompt(input, output, "Port", strconv.Itoa(candidate.Port))
	if err != nil {
		return config.DatabaseConfig{}, err
	}
	candidate.Port, err = strconv.Atoi(port)
	if err != nil {
		return config.DatabaseConfig{}, errors.New("database port must be a number between 1 and 65535")
	}

	candidate.User, err = prompt(input, output, "Username", candidate.User)
	if err != nil {
		return config.DatabaseConfig{}, err
	}

	newSetupOutput(output).ask("Password", "hidden")
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
	save, err := confirm(input, output, "Save credentials locally for this scope?", false)
	if err != nil {
		return nil, err
	}
	if !save {
		return nil, errors.New("configuration canceled; no settings were changed")
	}

	return selected, nil
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
	configureCmd.Flags().BoolVar(&configureRestoreAccess, "restore-access", false, "explicitly authorize a separate local restore provisioning account")
	configureCmd.MarkFlagsMutuallyExclusive("key", "restore-access")
	rootCmd.AddCommand(configureCmd)
}
