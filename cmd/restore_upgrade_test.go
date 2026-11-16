package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
)

func TestRestoreUpgradeNoticePrintsSQLWithoutChangingSettingsOrBlockingUpdates(t *testing.T) {
	const statement = "GRANT TRIGGER ON `fm\\_restore\\_%`.* TO 'custom_restore'@'localhost' WITH GRANT OPTION;"
	for _, scenario := range []string{"missing", "granted", "disabled", "unavailable", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			settings := config.Config{
				AgentCredential: "private-agent-credential",
				Database:        config.DatabaseConfig{Password: "private-backup-password"},
				Restore:         config.RestoreConfig{Authorized: scenario != "disabled", User: "custom_restore", Password: "private-restore-password"},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "canceled" {
				cancel()
			}
			called := false
			check := func(ctx context.Context, backup config.DatabaseConfig, restore config.RestoreConfig) error {
				called = true
				if backup.Password != settings.Database.Password || restore != settings.Restore {
					t.Fatal("permission check did not use the configured local account")
				}
				switch scenario {
				case "granted":
					return nil
				case "unavailable":
					return errors.New("private-server-response private-restore-password")
				default:
					return &database.RestoreTriggerGrantError{SQL: statement}
				}
			}
			var output bytes.Buffer
			printRestoreUpgradeNotice(ctx, &output, settings, check)
			if called != (scenario != "disabled" && scenario != "canceled") {
				t.Fatal("permission check ran without restore authorization or after cancellation")
			}
			text := output.String()
			for _, secret := range []string{"private-agent-credential", "private-backup-password", "private-restore-password", "private-server-response"} {
				if strings.Contains(text, secret) {
					t.Fatal("upgrade notice exposed credentials or raw server errors")
				}
			}
			switch scenario {
			case "missing":
				for _, want := range []string{statement, "database administrator", "table/view-only restores", "does not run this SQL", "Do not delete"} {
					if !strings.Contains(text, want) {
						t.Fatalf("upgrade notice omitted required guidance: %s", want)
					}
				}
			case "unavailable":
				if !strings.Contains(text, "does not block the update") || strings.Contains(text, "GRANT") {
					t.Fatal("unavailable database blocked an update or produced unverified SQL")
				}
			default:
				if text != "" {
					t.Fatal("permission upgrade notice was printed when it was not needed")
				}
			}
		})
	}
}
