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
					"[2/3] Check access", "[OK] Connection and restore permissions verified.",
					"[3/3] Authorize future restores", "SQL users server-wide (CREATE USER)",
					"Type AUTHORIZE", "[OK] Restore access saved on this machine.",
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
			if err == nil || !strings.Contains(output.String(), "[WARN]") || !strings.Contains(output.String(), "separate") {
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
