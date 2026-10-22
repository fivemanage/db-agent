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

type restoreInstallSetup struct {
	inspect   func(context.Context, config.DatabaseConfig) database.Report
	provision func(context.Context, config.DatabaseConfig, config.DatabaseConfig, string, func(config.RestoreConfig) error) error
	check     func(context.Context, config.DatabaseConfig, config.RestoreConfig) error
	save      func(config.RestoreConfig) error
}

func configureInstallRestoreAccess(cmd *cobra.Command, input *bufio.Reader, admin config.DatabaseConfig) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("install requires an interactive local terminal for dashboard restore setup; backup settings are preserved")
	}
	settings, err := config.LoadDiagnostic(configPath)
	if err != nil {
		return err
	}
	return setupDashboardRestores(cmd.Context(), input, cmd.OutOrStdout(), settings, admin,
		func() (string, error) { value, err := term.ReadPassword(int(os.Stdin.Fd())); return string(value), err },
		restoreInstallSetup{
			inspect:   database.Inspect,
			provision: database.ProvisionRestoreAccount,
			check:     database.CheckRestoreAccess,
			save: func(access config.RestoreConfig) error {
				return config.SaveRestoreAccess(configPath, settings.Database, access)
			},
		})
}

func setupDashboardRestores(ctx context.Context, input *bufio.Reader, output io.Writer, settings config.Config, admin config.DatabaseConfig, readSecret func() (string, error), setup restoreInstallSetup) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	view := newSetupOutput(output)
	view.section("Dashboard restores (optional)")
	verify := func(access config.RestoreConfig) error {
		if err := access.ValidateFor(settings.Database); err != nil {
			return err
		}
		return safeRestoreCheck(setup.check(ctx, settings.Database, access))
	}
	if settings.Restore.Authorized {
		if err := verify(settings.Restore); err != nil {
			return fmt.Errorf("existing restore authorization could not be verified: %w; settings are preserved; repair it with db-agent configure --restore-access", err)
		}
		view.status("OK", "Existing dashboard restore access verified and preserved.")
		return nil
	}
	view.status("INFO", "Dashboard restores use your local recovery key and import into a new isolated database, never over production.")
	view.status("INFO", "A separate local restore account is required. Your backup account stays read-only; database credentials never leave this machine.")
	enable, err := prompt(input, output, "Enable dashboard restores? [y/N]", "N")
	if err != nil {
		return err
	}
	if !affirmative(enable) {
		view.status("INFO", "Dashboard restores remain disabled. Backup-only setup will continue; rerun install to enable them later.")
		return nil
	}
	view.status("WARN", "Restore access includes sensitive global CREATE USER account-management authority, not limited to agent-owned users. Data grants are restricted to the fm_restore_ namespace. This account must never execute backup SQL directly.")
	persist := func(access config.RestoreConfig) error {
		if err := verify(access); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := setup.save(access); err != nil {
			return errors.New("cannot save private restore authorization; inspect the local config before retrying; backup settings and recovery key are preserved")
		}
		return nil
	}
	if settings.Restore.User != "" {
		access := settings.Restore
		access.Authorized = true
		if err := verify(access); err != nil {
			return fmt.Errorf("saved restore account could not be verified: %w; use db-agent configure --restore-access to repair it", err)
		}
		confirmed, err := confirmInstallRestores(input, output, "Type AUTHORIZE to re-enable the verified existing restore account")
		if err != nil || !confirmed {
			return err
		}
		if err := persist(access); err != nil {
			return err
		}
	} else {
		if err := createInstallRestoreAccount(ctx, input, output, settings.Database, admin, readSecret, setup, persist); err != nil {
			return fmt.Errorf("dashboard restore setup incomplete: %w; backup settings are preserved; rerun db-agent install to resume or continue backup-only", err)
		}
		return nil
	}
	view.status("OK", "Dashboard restore access saved privately. Backup access and the recovery key are unchanged; no restore was run.")
	return nil
}

func createInstallRestoreAccount(ctx context.Context, input *bufio.Reader, output io.Writer, backup, admin config.DatabaseConfig, readSecret func() (string, error), setup restoreInstallSetup, persist func(config.RestoreConfig) error) error {
	view := newSetupOutput(output)
	var report database.Report
	if admin.User != "" {
		report = setup.inspect(ctx, admin)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if !report.CanCreateUser {
		view.status("INFO", "A local administrator is needed once to create the separate restore account. Admin credentials are used in memory only and never saved.")
		admin = backup
		var err error
		admin.User, err = prompt(input, output, "Local database administrator username", "root")
		if err != nil {
			return err
		}
		fmt.Fprint(output, "Local administrator password (hidden): ")
		admin.Password, err = readSecret()
		fmt.Fprintln(output)
		if err != nil {
			return errors.New("cannot read administrator password from the local terminal; no restore account was created")
		}
		report = setup.inspect(ctx, admin)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(report.Checks) < 4 || !report.CanCreateUser || report.Driver != backup.Driver {
		return errors.New("administrator connection or CREATE USER permission could not be verified; no restore account was created")
	}
	if !printChecks(output, report.Checks[:4]) {
		return errors.New("administrator connection checks failed; no restore account was created")
	}
	admin.Driver = report.Driver
	statements, err := database.LocalRestoreAccountSQL(backup, report.ClientHost)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Separate restore account SQL for the exact server-observed client host %q (existing accounts are never modified):\n", report.ClientHost)
	for _, statement := range statements {
		fmt.Fprintf(output, "    %s;\n", statement)
	}
	confirmed, err := confirmInstallRestores(input, output, "Type AUTHORIZE to create this separate account and enable dashboard restores")
	if err != nil || !confirmed {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := setup.provision(ctx, admin, backup, report.ClientHost, persist); err != nil {
		return err
	}
	view.status("OK", "Dashboard restore account created, verified, and saved privately. Backup access and the recovery key are unchanged; no restore was run.")
	return nil
}

func confirmInstallRestores(input *bufio.Reader, output io.Writer, message string) (bool, error) {
	answer, err := prompt(input, output, message, "")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(answer) != "AUTHORIZE" {
		newSetupOutput(output).status("INFO", "Restore authorization not confirmed. No restore account or authorization was changed; backup-only setup will continue.")
		return false, nil
	}
	return true, nil
}

func safeRestoreCheck(err error) error {
	switch {
	case err == nil:
		return nil
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
