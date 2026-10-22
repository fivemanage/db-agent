package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
)

func installRestoreSettings() config.Config {
	return config.Config{Database: config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "backup-private", Databases: []string{"game_db"}}}
}

func TestInstallOffersBackupOnlyWithoutInspectingOrChangingRestoreAccess(t *testing.T) {
	for _, answer := range []string{"\n", "n\n"} {
		var output bytes.Buffer
		err := setupDashboardRestores(context.Background(), bufio.NewReader(strings.NewReader(answer)), &output, installRestoreSettings(), config.DatabaseConfig{},
			func() (string, error) { t.Fatal("declined setup asked for admin credentials"); return "", nil }, restoreInstallSetup{})
		if err != nil || !strings.Contains(output.String(), "Enable dashboard restores? [y/N]") || !strings.Contains(output.String(), "Backup-only setup will continue") {
			t.Fatalf("backup-only choice failed: %v", err)
		}
	}
}

func TestInstallCreatesSeparateRestoreAccountOnlyAfterExplicitApproval(t *testing.T) {
	for _, scenario := range []string{"new_install", "resume", "existing_backup", "decline_authorization", "eof", "bad_admin", "no_create_user", "unknown_host", "bad_grants", "raw_check_error", "save_error", "secret_error", "canceled", "cancel_during_inspect", "cancel_during_check"} {
		t.Run(scenario, func(t *testing.T) {
			settings := installRestoreSettings()
			admin := settings.Database
			admin.User, admin.Password = "root", "admin-private"
			input := "y\nAUTHORIZE\n"
			if scenario == "resume" || scenario == "secret_error" {
				admin = config.DatabaseConfig{}
				input = "y\nroot\nAUTHORIZE\n"
			}
			if scenario == "existing_backup" {
				admin = settings.Database
				input = "y\nroot\nAUTHORIZE\n"
			}
			if scenario == "decline_authorization" {
				input = "y\nn\n"
			}
			if scenario == "eof" {
				input = "y\n"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "canceled" {
				cancel()
			}
			var output bytes.Buffer
			created, checked, saved, secrets := false, false, false, 0
			setup := restoreInstallSetup{
				inspect: func(_ context.Context, candidate config.DatabaseConfig) database.Report {
					if candidate.User == "root" && candidate.Password != "admin-private" {
						t.Fatal("admin password lost")
					}
					r := setupReport(candidate.User == "root" && scenario != "no_create_user")
					r.ClientHost = "172.19.0.1"
					if scenario == "bad_admin" {
						r.Checks[1].Passed = false
					}
					if scenario == "unknown_host" {
						r.ClientHost = ""
					}
					if scenario == "cancel_during_inspect" {
						cancel()
					}
					return r
				},
				provision: func(_ context.Context, supplied, backup config.DatabaseConfig, host string, persist func(config.RestoreConfig) error) error {
					created = true
					text := output.String()
					if supplied.User != "root" || supplied.Password != "admin-private" || backup.User != "backup" || host != "172.19.0.1" {
						t.Fatal("restore setup did not preserve distinct admin and backup identities")
					}
					for _, want := range []string{"CREATE USER 'db_agent_restore'@'172.19.0.1' IDENTIFIED BY '<generated password>'", "GRANT CREATE USER ON *.*", "ON `fm\\_restore\\_%`.*", "WITH GRANT OPTION", "Type AUTHORIZE", "not limited to agent-owned users"} {
						if !strings.Contains(text, want) {
							t.Fatalf("missing permission preview %q", want)
						}
					}
					return persist(config.RestoreConfig{Authorized: true, Driver: backup.Driver, Host: backup.Host, Port: backup.Port, User: "db_agent_restore", Password: "restore-private"})
				},
				check: func(_ context.Context, backup config.DatabaseConfig, access config.RestoreConfig) error {
					checked = true
					if !created || access.ValidateFor(backup) != nil {
						t.Fatal("verification preceded creation or reused backup access")
					}
					if scenario == "bad_grants" {
						return database.ErrRestorePrivileges
					}
					if scenario == "raw_check_error" {
						return errors.New("restore-private raw checker failure")
					}
					if scenario == "cancel_during_check" {
						cancel()
					}
					return nil
				},
				save: func(access config.RestoreConfig) error {
					saved = true
					if !checked || access.User != "db_agent_restore" || access.Password != "restore-private" {
						t.Fatal("unverified or admin credentials persisted")
					}
					if scenario == "save_error" {
						return errors.New("admin-private restore-private raw storage failure")
					}
					return nil
				},
			}
			err := setupDashboardRestores(ctx, bufio.NewReader(strings.NewReader(input)), &output, settings, admin, func() (string, error) {
				secrets++
				if scenario == "secret_error" {
					return "", errors.New("admin-private")
				}
				return "admin-private", nil
			}, setup)
			success := scenario == "new_install" || scenario == "resume" || scenario == "existing_backup"
			wantCreate := success || scenario == "bad_grants" || scenario == "raw_check_error" || scenario == "save_error" || scenario == "cancel_during_check"
			if (err == nil) != (success || scenario == "decline_authorization") || created != wantCreate || checked != wantCreate || saved != (success || scenario == "save_error") {
				t.Fatalf("wrong setup result: created=%t checked=%t saved=%t err=%v", created, checked, saved, err)
			}
			if scenario == "new_install" && secrets != 0 || (scenario == "resume" || scenario == "existing_backup") && secrets != 1 {
				t.Fatal("fresh admin credentials were not reused, or resumed setup did not request them once")
			}
			for _, secret := range []string{"admin-private", "backup-private", "restore-private"} {
				if strings.Contains(output.String(), secret) || err != nil && strings.Contains(err.Error(), secret) {
					t.Fatal("restore setup leaked credentials or raw errors")
				}
			}
			if (scenario == "canceled" || strings.HasPrefix(scenario, "cancel_during")) && !errors.Is(err, context.Canceled) {
				t.Fatal("canceled setup continued")
			}
		})
	}
}

func TestInstallPreservesAndRechecksExistingRestoreAuthorization(t *testing.T) {
	for _, scenario := range []string{"enabled", "disabled", "revoked", "wrong_server"} {
		t.Run(scenario, func(t *testing.T) {
			settings := installRestoreSettings()
			settings.Restore = config.RestoreConfig{Authorized: scenario != "disabled", Driver: settings.Database.Driver, Host: settings.Database.Host, Port: settings.Database.Port, User: "existing_restore", Password: "restore-private"}
			if scenario == "wrong_server" {
				settings.Restore.Port++
			}
			var output bytes.Buffer
			checked, saved := false, false
			setup := restoreInstallSetup{
				check: func(_ context.Context, backup config.DatabaseConfig, access config.RestoreConfig) error {
					checked = true
					if access.User != settings.Restore.User || access.Password != settings.Restore.Password || !access.Authorized {
						t.Fatal("existing restore credentials were replaced")
					}
					if scenario == "revoked" {
						return database.ErrRestorePrivileges
					}
					return nil
				},
				save: func(config.RestoreConfig) error { saved = true; return nil },
			}
			err := setupDashboardRestores(context.Background(), bufio.NewReader(strings.NewReader("y\nAUTHORIZE\n")), &output, settings, config.DatabaseConfig{},
				func() (string, error) { t.Fatal("existing authorization asked for admin credentials"); return "", nil }, setup)
			if (err == nil) != (scenario == "enabled" || scenario == "disabled") || checked != (scenario != "wrong_server") || saved != (scenario == "disabled") {
				t.Fatalf("existing authorization changed or was not checked: saved=%t checked=%t err=%v", saved, checked, err)
			}
			if scenario == "enabled" && strings.Contains(output.String(), "Enable dashboard restores?") {
				t.Fatal("enabled restore access asked for setup again")
			}
		})
	}
}

func TestInstallRestoreFailureDoesNotRetryOrRollBackSuccessfulBackupSetup(t *testing.T) {
	var output bytes.Buffer
	saved, restored := false, false
	restoreErr := errors.New("restore setup failed")
	setup := databaseSetup{
		inspect: func(_ context.Context, candidate config.DatabaseConfig) database.Report {
			return setupReport(candidate.User == "root")
		},
		provision: func(_ context.Context, admin config.DatabaseConfig, _, _, _ string, persist func(config.DatabaseConfig) error) error {
			backup := admin
			backup.User, backup.Password = "backup", "backup-private"
			return persist(backup)
		},
		save: func(config.DatabaseConfig) error { saved = true; return nil },
		restore: func(admin config.DatabaseConfig) error {
			restored = true
			if !saved || admin.User != "root" || admin.Password != "admin-private" {
				t.Fatal("restore setup ran before backup save or lost temporary admin credentials")
			}
			return restoreErr
		},
	}
	err := configureDatabase(context.Background(), bufio.NewReader(strings.NewReader("1\ny\ny\n")), &output, config.DatabaseConfig{},
		func() (string, error) { return "mysql://root:admin-private@127.0.0.1/game_db", nil }, setup)
	if !errors.Is(err, restoreErr) || !saved || !restored || strings.Contains(output.String(), "Try another connection") {
		t.Fatalf("restore failure retried or rolled back backup setup: %v", err)
	}
}
