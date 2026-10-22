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
	fmt.Fprintln(output, "Restore access is optional and separate from the read-only backup account. No backup grants, keys, users, or databases are changed by this command.")
	fmt.Fprintln(output, "Supply an existing dedicated local provisioning account, not root or a game-server/backup account. A local administrator must grant only this template's permissions for the exact server-observed client host:")
	for _, statement := range database.RestoreAccessSQL() {
		fmt.Fprintf(output, "%s;\n", statement)
	}
	fmt.Fprintln(output, "CREATE USER is global SQL account-management authority, not a destination-only privilege. Use a dedicated account and protect it accordingly. These credentials must never execute backup SQL; actual imports require a separate destination-confined principal. Credentials stay in the private local config and are never sent to FiveManage.")
	user, err := prompt(input, output, "Dedicated restore provisioning username", "")
	if err != nil {
		return err
	}
	fmt.Fprint(output, "Restore provisioning password (hidden): ")
	password, err := readSecret()
	fmt.Fprintln(output)
	if err != nil {
		return errors.New("cannot read restore password from the local terminal; no authorization was saved")
	}
	access := config.RestoreConfig{Authorized: true, Driver: backup.Driver, Host: backup.Host, Port: backup.Port, User: user, Password: password}
	if err := access.ValidateFor(backup); err != nil {
		return err
	}
	test, err := prompt(input, output, "Verify this separate account with read-only connection/grant checks? [y/N]", "N")
	if err != nil {
		return err
	}
	if !affirmative(test) {
		return errors.New("restore authorization canceled; existing settings are preserved")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := check(ctx, backup, access); err != nil {
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
	confirm, err := prompt(input, output, "Type AUTHORIZE to save these separate credentials locally for future restore provisioning", "")
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
	fmt.Fprintln(output, "Separate restore provisioning access saved privately. Backup credentials and the recovery key are unchanged. No restore was run; dashboard import is not enabled by this setup command.")
	return nil
}
