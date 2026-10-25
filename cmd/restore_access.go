package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func configureLocalRestoreAccess(cmd *cobra.Command, settings config.Config) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("restore-access setup requires an interactive local terminal for hidden password entry; no authorization was saved")
	}
	return authorizeRestoreAccess(cmd.Context(), bufio.NewReader(cmd.InOrStdin()), cmd.OutOrStdout(), settings.Database,
		func() (string, error) { value, err := term.ReadPassword(int(os.Stdin.Fd())); return string(value), err },
		database.CheckRestoreAccess,
		func(access config.RestoreConfig) error {
			return config.SaveRestoreAccess(configPath, settings.Database, access)
		})
}

func authorizeRestoreAccess(ctx context.Context, input *bufio.Reader, output io.Writer, backup config.DatabaseConfig,
	readSecret func() (string, error), check func(context.Context, config.DatabaseConfig, config.RestoreConfig) error, save func(config.RestoreConfig) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	view := newSetupOutput(output)
	fmt.Fprintln(output, "\nRestores go into a new, separate database, not your live game database.")
	fmt.Fprintln(output, "This optional setup only checks and saves a restore account on this machine.")
	fmt.Fprintln(output, "It does not change SQL users, databases, backup access, or your recovery key.")
	view.step(1, 3, "Restore account")
	fmt.Fprintln(output, "  Not root, your game-server login, or your read-only backup login.")
	fmt.Fprintln(output, "  Need an account? Enter ? for instructions for your database administrator.")
	user, err := promptRestoreUser(ctx, input, output, backup.User)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "  The password stays in your private local config, never sent to FiveManage.")
	view.ask("Restore account password", "hidden")
	password, err := readSecret()
	fmt.Fprintln(output)
	if err != nil {
		return errors.New("cannot read restore password from the local terminal; no authorization was saved")
	}
	access := config.RestoreConfig{Authorized: true, Driver: backup.Driver, Host: backup.Host, Port: backup.Port, User: user, Password: password}
	if err := access.ValidateFor(backup); err != nil {
		return err
	}
	view.step(2, 3, "Check access")
	test, err := confirm(input, output, "Check this account now?", false)
	if err != nil {
		return err
	}
	if !test {
		return errors.New("restore authorization canceled; existing settings are preserved")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := view.spin("Checking restore account")
	err = check(ctx, backup, access)
	stop()
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		case errors.Is(err, config.ErrRestoreAuthorization):
			return config.ErrRestoreAuthorization
		case errors.Is(err, database.ErrRestoreUnavailable):
			return database.ErrRestoreUnavailable
		case errors.Is(err, database.ErrRestorePrivileges):
			return database.ErrRestorePrivileges
		default:
			return errors.New("cannot verify local restore provisioning access; no authorization was saved")
		}
	}
	view.ok("Connection and restore permissions verified.")
	view.step(3, 3, "Authorize future restores")
	fmt.Fprintln(output, "  - Can create and manage SQL users server-wide (CREATE USER).")
	fmt.Fprintln(output, "    This is a powerful permission. Keep this account private.")
	fmt.Fprintln(output, "  - Can create databases and grant access within fm_restore_ only.")
	fmt.Fprintln(output, "  - Restored SQL uses a different account limited to its new database.")
	fmt.Fprintln(output, "\nNothing is saved unless you authorize it. No restore will run now.")
	confirm, err := prompt(input, output, "Type AUTHORIZE to save restore access", "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(confirm) != "AUTHORIZE" {
		return errors.New("restore authorization not confirmed; existing settings are preserved")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := save(access); err != nil {
		return errors.New("cannot finish saving private restore authorization; inspect the local config before retrying; no restore was run")
	}
	view.ok("Restore access saved on this machine.")
	fmt.Fprintln(output, "  Backup access and your recovery key are unchanged. No restore was run.")
	return nil
}

func promptRestoreUser(ctx context.Context, input *bufio.Reader, output io.Writer, backupUser string) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		user, err := prompt(input, output, "Restore account username (or ? for help)", "")
		if err != nil {
			return "", err
		}
		switch {
		case user == "?":
			printRestoreAccountHelp(output)
		case user == "":
			newSetupOutput(output).warn("Enter the restore account username, or ? for setup instructions.")
		case strings.EqualFold(user, "root"):
			newSetupOutput(output).warn("Do not use root here. Enter a separate account created only for restores.")
		case strings.EqualFold(user, backupUser):
			newSetupOutput(output).warn("That is your backup account. Keep it read-only and use a separate restore account.")
		default:
			return user, nil
		}
	}
}

func printRestoreAccountHelp(output io.Writer) {
	newSetupOutput(output).section("Create a dedicated restore account (administrator task)")
	fmt.Fprintln(output, "  Ask your database administrator to run the SQL below, then return here.")
	fmt.Fprintln(output, "  If the dedicated account already exists, verify its grants; do not recreate it.")
	fmt.Fprintln(output, "\n  Replace the placeholders first:")
	fmt.Fprintln(output, "  - <private restore password>: a new, strong password used only here.")
	fmt.Fprintln(output, "  - <exact local client host>: the host MySQL/MariaDB sees for this agent.")
	fmt.Fprintln(output, "    Check USER() from a connection using the agent's local TCP address.")
	fmt.Fprintln(output, "    Docker may report a bridge IP. Never use % or a subnet as the host.")
	fmt.Fprintln(output, "\n  Grant only these permissions, with no roles or production database access.")
	fmt.Fprintln(output, "  CREATE USER allows SQL user management server-wide. Protect this account.")
	wrap := strings.NewReplacer(" IDENTIFIED BY ", "\n      IDENTIFIED BY ", " ON ", "\n      ON ", " TO ", "\n      TO ", ", REFERENCES", ",\n      REFERENCES")
	for _, statement := range database.RestoreAccessSQL() {
		fmt.Fprintf(output, "\n    %s;\n", wrap.Replace(statement))
	}
	fmt.Fprintln(output, "\n  Once the account is ready, enter its username below (db_agent_restore).")
}
