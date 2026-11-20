package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/key"
)

func TestConfigureReusesSavedRestoreAccessWithoutDatabaseSetup(t *testing.T) {
	for _, scenario := range []string{"enabled", "disabled", "bad_grants", "declined", "cancel", "eof", "invalid", "wrong_server"} {
		t.Run(scenario, func(t *testing.T) {
			settings := installRestoreSettings()
			settings.Restore = config.RestoreConfig{
				Authorized: scenario == "enabled", Driver: settings.Database.Driver,
				Host: settings.Database.Host, Port: settings.Database.Port, User: "saved_restore", Password: "restore-private",
			}
			input := "2\n1\ny\n"
			switch scenario {
			case "declined":
				input = "2\n1\nn\n"
			case "cancel":
				input = "2\n\n"
			case "eof":
				input = "2\n"
			case "invalid":
				input = "2\n9\n"
			case "wrong_server":
				settings.Restore.Port++
			}
			reader := bufio.NewReader(strings.NewReader(input))
			var output bytes.Buffer
			choice, err := promptConfigureAction(reader, &output, settings)
			if err != nil || choice != "2" {
				t.Fatal("restore action did not preserve the settings-menu input")
			}
			checked, saved := false, false
			err = configureRestoreAccounts(context.Background(), reader, &output, settings,
				func() (string, error) { t.Fatal("saved restore access asked for credentials"); return "", nil },
				restoreInstallSetup{
					inspect: func(context.Context, config.DatabaseConfig) database.Report {
						t.Fatal("restore settings entered the database connection flow")
						return database.Report{}
					},
					provision: func(context.Context, config.DatabaseConfig, config.DatabaseConfig, string, func(config.RestoreConfig) error) error {
						t.Fatal("saved restore setup created or changed a SQL user")
						return nil
					},
					check: func(_ context.Context, backup config.DatabaseConfig, access config.RestoreConfig) error {
						checked = true
						if !reflect.DeepEqual(backup, settings.Database) || !access.Authorized || access.User != "saved_restore" || access.Password != "restore-private" {
							t.Fatal("saved connection or restore credentials were changed")
						}
						if scenario == "bad_grants" {
							return database.ErrRestorePrivileges
						}
						return nil
					},
					save: func(config.RestoreConfig) error { saved = true; return nil },
				})
			wantCheck := scenario == "enabled" || scenario == "disabled" || scenario == "bad_grants"
			wantSuccess := scenario == "enabled" || scenario == "disabled" || scenario == "declined" || scenario == "cancel"
			if checked != wantCheck || saved != (scenario == "disabled") || (err == nil) != wantSuccess {
				t.Fatalf("wrong restore setup result: checked=%t saved=%t err=%v", checked, saved, err)
			}
			for _, secret := range []string{settings.Database.Password, settings.Restore.Password} {
				if strings.Contains(output.String(), secret) || err != nil && strings.Contains(err.Error(), secret) {
					t.Fatal("restore settings disclosed a password")
				}
			}
		})
	}
}

func TestConfigureCanCreateRestoreAccountUsingOnlyFreshAdminCredentials(t *testing.T) {
	for _, approve := range []bool{false, true} {
		settings := installRestoreSettings()
		input := "1\nn\n"
		if approve {
			input = "1\ny\nadmin\n"
		}
		var output bytes.Buffer
		created, checked, saved, secrets := false, false, false, 0
		err := configureRestoreAccounts(context.Background(), bufio.NewReader(strings.NewReader(input)), &output, settings,
			func() (string, error) { secrets++; return "admin-private", nil },
			restoreInstallSetup{
				inspect: func(_ context.Context, admin config.DatabaseConfig) database.Report {
					if admin.User != "admin" || admin.Password != "admin-private" || admin.Host != settings.Database.Host || admin.Port != settings.Database.Port || !reflect.DeepEqual(admin.Databases, settings.Database.Databases) {
						t.Fatal("restore creation did not use fresh admin credentials on the saved server")
					}
					return setupReport(true)
				},
				provision: func(_ context.Context, admin, backup config.DatabaseConfig, host string, persist func(config.RestoreConfig) error) error {
					created = true
					if !approve || !reflect.DeepEqual(backup, settings.Database) || host != "127.0.0.1" {
						t.Fatal("restore creation changed backup settings or lacked approval")
					}
					return persist(config.RestoreConfig{Authorized: true, Driver: backup.Driver, Host: backup.Host, Port: backup.Port, User: "db_agent_restore", Password: "restore-private"})
				},
				check: func(_ context.Context, backup config.DatabaseConfig, access config.RestoreConfig) error {
					checked = true
					return access.ValidateFor(backup)
				},
				save: func(access config.RestoreConfig) error {
					saved = true
					if !checked || access.User != "db_agent_restore" || access.Password != "restore-private" {
						t.Fatal("admin or unverified restore credentials were saved")
					}
					return nil
				},
			})
		wantSecrets := 0
		if approve {
			wantSecrets = 1
		}
		if err != nil || created != approve || checked != approve || saved != approve || secrets != wantSecrets {
			t.Fatalf("wrong restore creation result: created=%t checked=%t saved=%t secrets=%d err=%v", created, checked, saved, secrets, err)
		}
		for _, secret := range []string{"admin-private", "restore-private", settings.Database.Password} {
			if strings.Contains(output.String(), secret) {
				t.Fatal("restore creation disclosed a password")
			}
		}
	}
}

func TestConfigureExistingRestoreAccountPreservesOtherConfigAndKey(t *testing.T) {
	path := setupConfig(t)
	keyPath := filepath.Join(filepath.Dir(path), "encryption.key")
	k, _, err := key.Ensure(keyPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveKeyBinding(path, keyPath, k.ID(), true); err != nil {
		t.Fatal(err)
	}
	before, err := config.LoadDiagnostic(path)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	setup := restoreAccountSetup(path, before.Database)
	setup.inspect = func(context.Context, config.DatabaseConfig) database.Report {
		t.Fatal("existing-account setup inspected an admin connection")
		return database.Report{}
	}
	setup.provision = func(context.Context, config.DatabaseConfig, config.DatabaseConfig, string, func(config.RestoreConfig) error) error {
		t.Fatal("existing-account setup changed SQL users")
		return nil
	}
	setup.check = func(_ context.Context, backup config.DatabaseConfig, access config.RestoreConfig) error {
		if !reflect.DeepEqual(backup, before.Database) {
			t.Fatal("existing-account setup changed the saved backup connection")
		}
		return access.ValidateFor(backup)
	}
	var output bytes.Buffer
	err = configureRestoreAccounts(context.Background(), bufio.NewReader(strings.NewReader("2\nrecreated_restore\ny\nAUTHORIZE\n")), &output, before,
		func() (string, error) { return "new-restore-private", nil }, setup)
	if err != nil {
		t.Fatal(err)
	}
	after, err := config.LoadDiagnostic(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Restore.Authorized || after.Restore.User != "recreated_restore" || after.Restore.Password != "new-restore-private" {
		t.Fatal("existing restore account was not saved")
	}
	after.Restore = before.Restore
	keyAfter, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(keyBefore, keyAfter) || !reflect.DeepEqual(before, after) {
		t.Fatal("restore configuration changed unrelated settings or the recovery key")
	}
	if strings.Contains(output.String(), "Connection string") || strings.Contains(output.String(), "new-restore-private") {
		t.Fatal("existing restore setup requested the connection string or disclosed its password")
	}
}

func TestRestoreSetupRequiresExplicitApprovalAndNeverEchoesSecrets(t *testing.T) {
	for _, scenario := range []string{"success", "decline_test", "decline_authorization", "bad_grants", "check_timeout", "raw_check_error", "save_error", "secret_error", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			backup := config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "backup-private", Databases: []string{"game"}}
			input := "restore_setup\ny\nAUTHORIZE\n"
			if scenario == "decline_test" {
				input = "restore_setup\nn\n"
			}
			if scenario == "decline_authorization" {
				input = "restore_setup\ny\nn\n"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancel" {
				cancel()
			}
			checked, saved := false, false
			var output bytes.Buffer
			err := authorizeRestoreAccess(ctx, bufio.NewReader(strings.NewReader(input)), &output, backup,
				func() (string, error) {
					if scenario == "secret_error" {
						return "", errors.New("restore-private")
					}
					return "restore-private", nil
				},
				func(_ context.Context, original config.DatabaseConfig, access config.RestoreConfig) error {
					checked = true
					if original.User != "backup" || access.User != "restore_setup" || access.Password != "restore-private" || !access.Authorized {
						t.Fatal("restore setup reused or changed the backup account")
					}
					if scenario == "bad_grants" {
						return database.ErrRestorePrivileges
					}
					if scenario == "check_timeout" {
						return context.DeadlineExceeded
					}
					if scenario == "raw_check_error" {
						return errors.New("restore-private raw checker failure")
					}
					return nil
				},
				func(config.RestoreConfig) error {
					saved = true
					if scenario == "save_error" {
						return errors.New("restore-private raw storage failure")
					}
					return nil
				})
			if (err == nil) != (scenario == "success") || saved != (scenario == "success" || scenario == "save_error") {
				t.Fatalf("wrong confirmation/save behavior: %v", err)
			}
			if scenario == "success" {
				text := output.String()
				previous := -1
				for _, want := range []string{
					"[1/3] Restore account", "Restore account password (hidden)",
					"[2/3] Check access", "+ Connection and restore permissions verified.",
					"[3/3] Authorize future restores", "SQL users server-wide (CREATE USER)",
					"Type AUTHORIZE", "+ Restore access saved on this machine.",
				} {
					position := strings.Index(text, want)
					if position <= previous {
						t.Fatalf("missing or out-of-order setup guidance %q: %s", want, text)
					}
					previous = position
				}
				if strings.Contains(text, "GRANT ") || strings.Contains(text, "IDENTIFIED BY") {
					t.Fatal("SQL instructions should only appear when requested")
				}
			}
			if checked != (scenario != "decline_test" && scenario != "secret_error" && scenario != "cancel") {
				t.Fatal("account checks ran without approval or after cancellation")
			}
			for _, secret := range []string{"restore-private", "backup-private"} {
				if strings.Contains(output.String(), secret) || err != nil && strings.Contains(err.Error(), secret) {
					t.Fatal("restore setup echoed credentials or raw errors")
				}
			}
		})
	}
}

func TestRestoreSetupRejectsBackupAndRootAccountsBeforePassword(t *testing.T) {
	backup := config.DatabaseConfig{Driver: "mariadb", Host: "localhost", Port: 3306, User: "backup", Databases: []string{"game"}}
	for _, user := range []string{"backup", "root", "ROOT", "BACKUP"} {
		t.Run(user, func(t *testing.T) {
			var output bytes.Buffer
			err := authorizeRestoreAccess(context.Background(), bufio.NewReader(strings.NewReader(user+"\n")), &output, backup,
				func() (string, error) { t.Fatal("unsafe account prompted for a password"); return "", nil },
				func(context.Context, config.DatabaseConfig, config.RestoreConfig) error {
					t.Fatal("unsafe account was connected")
					return nil
				},
				func(config.RestoreConfig) error { t.Fatal("unsafe account was saved"); return nil })
			if err == nil || !strings.Contains(output.String(), "  ! ") || !strings.Contains(output.String(), "separate") {
				t.Fatalf("unsafe account needs an actionable warning before retrying: %v, %s", err, output.String())
			}
			if strings.Contains(output.String(), "password (hidden)") {
				t.Fatal("unsafe account prompted for a password")
			}
		})
	}
}

func TestRestoreSetupCanShowHelpAndRetryUnsafeUser(t *testing.T) {
	backup := config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Databases: []string{"game"}}
	for _, input := range []string{"?\ndb_agent_restore\ny\nAUTHORIZE\n", "root\nbackup\n\ndb_agent_restore\ny\nAUTHORIZE\n"} {
		t.Run(strings.Split(input, "\n")[0], func(t *testing.T) {
			var output bytes.Buffer
			passwordReads, checks, saves := 0, 0, 0
			err := authorizeRestoreAccess(context.Background(), bufio.NewReader(strings.NewReader(input)), &output, backup,
				func() (string, error) { passwordReads++; return "private", nil },
				func(_ context.Context, _ config.DatabaseConfig, access config.RestoreConfig) error {
					checks++
					if access.User != "db_agent_restore" {
						t.Fatal("checked an unsafe account")
					}
					return nil
				},
				func(access config.RestoreConfig) error {
					saves++
					if access.User != "db_agent_restore" {
						t.Fatal("saved an unsafe account")
					}
					return nil
				})
			if err != nil || passwordReads != 1 || checks != 1 || saves != 1 {
				t.Fatalf("setup did not resume safely: %v (passwords=%d, checks=%d, saves=%d)", err, passwordReads, checks, saves)
			}
			if strings.HasPrefix(input, "?") {
				for _, want := range []string{"administrator task", "USER()", "Never use % or a subnet", "GRANT CREATE USER", "WITH GRANT OPTION", "<private restore password>", "<exact local client host>"} {
					if !strings.Contains(output.String(), want) {
						t.Fatalf("missing administrator guidance %q", want)
					}
				}
			}
		})
	}
}
