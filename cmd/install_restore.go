package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

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
	verify := func(access config.RestoreConfig) error {
		if err := access.ValidateFor(settings.Database); err != nil {
			return err
		}
		return safeRestoreCheck(setup.check(ctx, settings.Database, access))
	}
	if settings.Restore.Authorized {
		stop := view.spin("Checking dashboard restore access")
		err := verify(settings.Restore)
		stop()
		if err != nil {
			return fmt.Errorf("existing restore authorization could not be verified: %w; settings are preserved; repair it with db-agent configure --restore-access", err)
		}
		view.ok("Dashboard restores enabled")
		return nil
	}

	view.section("Dashboard restores (optional)")
	view.note("Restore backups from the dashboard into a new database. Your live database is never touched.")
	view.note("This needs a separate restore user that can create databases and users; your backup user stays read-only.")
	question := "Create a restore user?"
	if settings.Restore.User != "" {
		question = "Turn dashboard restores back on with your saved restore user?"
	}
	enable, err := confirm(input, output, question, false)
	if err != nil {
		return err
	}
	if !enable {
		view.info("Skipped. Run db-agent install again to enable dashboard restores later.")
		return nil
	}
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
		stop := view.spin("Checking your saved restore user")
		err := persist(access)
		stop()
		if err != nil {
			return fmt.Errorf("saved restore account could not be verified: %w; use db-agent configure --restore-access to repair it", err)
		}
		view.ok("Dashboard restores enabled")
		return nil
	}
	if err := createInstallRestoreAccount(ctx, input, output, settings.Database, admin, readSecret, setup, persist); err != nil {
		return fmt.Errorf("dashboard restore setup incomplete: %w; backup settings are preserved; rerun db-agent install to resume or continue backup-only", err)
	}
	return nil
}

func createInstallRestoreAccount(ctx context.Context, input *bufio.Reader, output io.Writer, backup, admin config.DatabaseConfig, readSecret func() (string, error), setup restoreInstallSetup, persist func(config.RestoreConfig) error) error {
	view := newSetupOutput(output)
	var report database.Report
	if admin.User != "" {
		stop := view.spin("Checking admin access")
		report = setup.inspect(ctx, admin)
		stop()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if !report.CanCreateUser {
		view.info("We need a database admin login once to create it. It's never saved.")
		admin = backup
		var err error
		admin.User, err = prompt(input, output, "Admin username", "root")
		if err != nil {
			return err
		}
		view.ask("Admin password", "hidden")
		admin.Password, err = readSecret()
		fmt.Fprintln(output)
		if err != nil {
			return errors.New("cannot read administrator password from the local terminal; no restore account was created")
		}
		stop := view.spin("Checking admin access")
		report = setup.inspect(ctx, admin)
		stop()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(report.Checks) >= 4 && !checksPassed(report.Checks[:4]) {
		printFailedChecks(output, report.Checks[:4])
		return errors.New("administrator connection checks failed; no restore account was created")
	}
	if len(report.Checks) < 4 || !report.CanCreateUser || report.Driver != backup.Driver {
		return errors.New("administrator connection or CREATE USER permission could not be verified; no restore account was created")
	}
	admin.Driver = report.Driver
	if _, err := database.LocalRestoreAccountSQL(backup, report.ClientHost); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := view.spin("Creating the restore user")
	err := setup.provision(ctx, admin, backup, report.ClientHost, persist)
	stop()
	if err != nil {
		return err
	}
	view.ok("Restore user created. Dashboard restores enabled")
	return nil
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
