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

func TestConnectionPromptNeverEchoesPasswordOrKeepsOldScope(t *testing.T) {
	var output bytes.Buffer
	previous := config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "old-secret", Databases: []string{"old-database"}}
	settings, err := promptConnection(bufio.NewReader(strings.NewReader("\n\n\n")), &output, previous, func() (string, error) { return "new-secret", nil })
	if err != nil || settings.Password != "new-secret" || settings.Host != previous.Host || len(settings.Databases) != 0 {
		t.Fatal("connection candidate did not reset scope or read hidden password")
	}
	if strings.Contains(output.String(), "old-secret") || strings.Contains(output.String(), "new-secret") {
		t.Fatal("password echoed in prompt")
	}
	_, err = promptConnection(bufio.NewReader(strings.NewReader("\n\n\n")), &output, previous, func() (string, error) { return "", errors.New("new-secret") })
	if err == nil || strings.Contains(err.Error(), "new-secret") {
		t.Fatal("raw password read error leaked")
	}
}

func setupReport(admin bool) database.Report {
	r := database.Report{Driver: "mysql", Version: "8.4.0", ClientHost: "127.0.0.1", CanCreateUser: admin, Administrative: admin}
	for _, name := range []string{"connectivity", "authentication", "version", "selection", "grants"} {
		r.Checks = append(r.Checks, database.Check{Name: name, Passed: true})
	}
	return r
}

func TestWizardNeverSavesSuppliedAdminCredentials(t *testing.T) {
	for _, test := range []struct {
		name, input    string
		saved, created bool
	}{
		{"approved SQL", "1\ny\ny\n", true, true},
		{"declined SQL", "1\ny\nn\nn\n", false, false},
		{"unconfirmed connection", "1\nn\nn\n", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			saved, created := false, false
			setup := databaseSetup{
				inspect: func(_ context.Context, c config.DatabaseConfig) database.Report { return setupReport(c.User == "root") },
				provision: func(_ context.Context, admin config.DatabaseConfig, _, _, _ string, persist func(config.DatabaseConfig) error) error {
					created = true
					if admin.User != "root" || admin.Password != "private-admin-password" {
						t.Fatal("wrong ephemeral account")
					}
					backup := admin
					backup.User, backup.Password = "db_agent_backup", "generated-private-password"
					return persist(backup)
				},
				save: func(c config.DatabaseConfig) error {
					saved = true
					if c.User != "db_agent_backup" || c.Password != "generated-private-password" {
						t.Fatal("admin settings saved")
					}
					return nil
				},
			}
			err := configureDatabase(context.Background(), bufio.NewReader(strings.NewReader(test.input)), &output, config.DatabaseConfig{}, func() (string, error) { return "mysql://root:private-admin-password@localhost/game_db", nil }, setup)
			if saved != test.saved || created != test.created || (err == nil) != test.saved {
				t.Fatalf("saved=%t created=%t err=%v", saved, created, err)
			}
			for _, secret := range []string{"private-admin-password", "generated-private-password"} {
				if strings.Contains(output.String(), secret) || err != nil && strings.Contains(err.Error(), secret) {
					t.Fatal("wizard leaked a secret")
				}
			}
		})
	}
}

func TestWizardRetriesFailedConnectionBeforeSavingExistingAccount(t *testing.T) {
	var output bytes.Buffer
	inspections, saved := 0, false
	setup := databaseSetup{
		inspect: func(context.Context, config.DatabaseConfig) database.Report {
			inspections++
			r := setupReport(false)
			if inspections == 1 {
				r.Checks[1].Passed = false
			}
			return r
		},
		provision: func(context.Context, config.DatabaseConfig, string, string, string, func(config.DatabaseConfig) error) error {
			t.Fatal("existing account path changed SQL")
			return nil
		},
		save: func(c config.DatabaseConfig) error {
			saved = true
			if c.User != "backup" {
				t.Fatal("wrong saved account")
			}
			return nil
		},
	}
	err := configureDatabase(context.Background(), bufio.NewReader(strings.NewReader("1\ny\ny\n1\ny\ny\n")), &output, config.DatabaseConfig{}, func() (string, error) { return "mysql://backup:private-backup-password@127.0.0.1/game", nil }, setup)
	if err != nil || !saved || inspections != 2 {
		t.Fatalf("retry failed: %v", err)
	}
	if strings.Contains(output.String(), "private-backup-password") {
		t.Fatal("backup password leaked")
	}
}

func TestWizardApprovesObservedClientHostBeforeProvisioning(t *testing.T) {
	for _, approve := range []bool{true, false} {
		var output bytes.Buffer
		created, saved := false, false
		setup := databaseSetup{
			inspect: func(_ context.Context, c config.DatabaseConfig) database.Report {
				r := setupReport(c.User == "root")
				r.ClientHost = "172.19.0.1"
				return r
			},
			provision: func(_ context.Context, admin config.DatabaseConfig, _, _, host string, persist func(config.DatabaseConfig) error) error {
				created = true
				if !approve || host != "172.19.0.1" || admin.Host != "localhost" {
					t.Fatal("unapproved host or incorrect destination passed to provisioning")
				}
				if !strings.Contains(output.String(), "CREATE USER 'db_agent_backup'@'172.19.0.1' IDENTIFIED BY '<generated password>';") ||
					strings.Contains(output.String(), "'db_agent_backup'@'localhost'") || strings.Contains(output.String(), "'db_agent_backup'@'%'") {
					t.Fatal("approved SQL does not match the exact bridge identity")
				}
				backup := admin
				backup.User, backup.Password = "db_agent_backup", "generated-private-password"
				return persist(backup)
			},
			save: func(config.DatabaseConfig) error { saved = true; return nil },
		}
		answer := "n\n"
		if approve {
			answer = "y\n"
		}
		err := setupDatabase(context.Background(), bufio.NewReader(strings.NewReader(answer)), &output, config.DatabaseConfig{Host: "localhost", User: "root", Databases: []string{"es_extended"}}, setup)
		if created != approve || saved != approve || (err == nil) != approve {
			t.Fatalf("approval not enforced: approved=%t created=%t saved=%t err=%v", approve, created, saved, err)
		}
		if strings.Contains(output.String(), "generated-private-password") {
			t.Fatal("generated password leaked")
		}
	}
}

func TestWizardDoesNotGuessUnknownClientHost(t *testing.T) {
	for _, admin := range []bool{true, false} {
		var output bytes.Buffer
		saved := false
		setup := databaseSetup{
			inspect: func(context.Context, config.DatabaseConfig) database.Report {
				r := setupReport(admin)
				r.ClientHost = ""
				return r
			},
			provision: func(context.Context, config.DatabaseConfig, string, string, string, func(config.DatabaseConfig) error) error {
				t.Fatal("account creation attempted without a discovered client host")
				return nil
			},
			save: func(config.DatabaseConfig) error { saved = true; return nil },
		}
		user := "backup"
		if admin {
			user = "root"
		}
		err := setupDatabase(context.Background(), bufio.NewReader(strings.NewReader("y\n")), &output, config.DatabaseConfig{Host: "localhost", User: user, Databases: []string{"game"}}, setup)
		if (err == nil) != !admin || saved != !admin || strings.Contains(output.String(), "CREATE USER") {
			t.Fatalf("unknown client host guessed or existing account blocked: admin=%t saved=%t err=%v", admin, saved, err)
		}
	}
}

func TestWizardRequiresExistingBackupAccountConfirmation(t *testing.T) {
	for _, confirm := range []string{"\n", "n\n"} {
		var output bytes.Buffer
		setup := databaseSetup{
			inspect: func(context.Context, config.DatabaseConfig) database.Report { return setupReport(false) },
			save:    func(config.DatabaseConfig) error { t.Fatal("unconfirmed account saved"); return nil },
		}
		if err := setupDatabase(context.Background(), bufio.NewReader(strings.NewReader(confirm)), &output, config.DatabaseConfig{User: "backup", Databases: []string{"game"}}, setup); err == nil {
			t.Fatal("confirmation not required")
		}
	}
}

func TestScopeConfirmationIsRequiredAndNamesAreEscaped(t *testing.T) {
	for _, input := range []string{"1\n\n", "1\nn\n", "1\n", "\n"} {
		var output bytes.Buffer
		if _, err := promptScope(bufio.NewReader(strings.NewReader(input)), &output, []string{"game"}); err == nil {
			t.Fatalf("accepted unconfirmed scope %q", input)
		}
	}
	var output bytes.Buffer
	selected, err := promptScope(bufio.NewReader(strings.NewReader("1\ny\n")), &output, []string{"game\x1b[31m"})
	if err != nil || len(selected) != 1 || strings.ContainsRune(output.String(), '\x1b') {
		t.Fatalf("unsafe scope prompt: selected=%v err=%v", selected, err)
	}
}

func TestScopeRequiresExplicitSelection(t *testing.T) {
	for _, input := range []string{"", "0", "3", "all", "1,"} {
		if _, err := selectDatabases(input, []string{"game", "other"}); err == nil {
			t.Fatalf("accepted selection %q", input)
		}
	}
	selected, err := selectDatabases("2,1,2", []string{"game", "other"})
	if err != nil || len(selected) != 2 || selected[0] != "other" || selected[1] != "game" {
		t.Fatalf("selection=%v err=%v", selected, err)
	}
}

func TestPrintChecksFailsOnSkippedPrerequisites(t *testing.T) {
	var output bytes.Buffer
	checks := []database.Check{{Name: "API", Passed: true, Message: "reached"}, {Name: "DB", Skipped: true, Message: "configure first"}}
	if printChecks(&output, checks) {
		t.Fatal("skipped DB check counted as passing")
	}
	if output.String() != "[PASS] API: reached\n[SKIP] DB: configure first\n" {
		t.Fatalf("output=%q", output.String())
	}
}
