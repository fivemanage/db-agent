package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/securefile"
)

func TestOverridesPreserveLocalSecretsAndUnknownSettingsDuringIncompleteSetup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	input := "agent_credential: dac_local\napi_url: https://api.example.com\nupdate_url: https://releases.example.com/feed\ndatabase:\n  password: local-private\nencryption:\n  key_file: /private/key\nbackup:\n  dump_timeout: 45m\n  custom_setting: preserve-me\nfuture_setting: untouched\n"
	if err := securefile.Create(path, []byte(input)); err != nil {
		t.Fatal(err)
	}
	overrides, err := DecodeDashboardOverrides([]byte(`{"backup":{"schedule":"0 5 * * *","compression":"gzip","retention_days":12,"throttle_io":false},"auto_update":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveDashboardOverrides(path, overrides); err != nil {
		t.Fatal(err)
	}
	c, err := LoadDiagnostic(path)
	if err != nil || c.AgentCredential != "dac_local" || c.Database.Password != "local-private" || c.Encryption.KeyFile != "/private/key" || c.UpdateURL != "https://releases.example.com/feed" || c.Backup.Schedule != "0 5 * * *" || c.Backup.DumpTimeout != "45m" || c.AutoUpdate {
		t.Fatal("override changed local settings or failed to update allowed policy")
	}
	contents, _ := os.ReadFile(path)
	for _, field := range []string{"preserve-me", "untouched"} {
		if !strings.Contains(string(contents), field) {
			t.Fatal("unknown local field lost")
		}
	}
}

func TestOverridesRejectSecretInjectionAndBadPolicyWithoutEchoingValues(t *testing.T) {
	for _, input := range []string{
		`{"agent_credential":"private-value"}`, `{"database":{"password":"private-value"}}`, `{"encryption":{"key_file":"private-value"}}`, `{"update_url":"private-value"}`, `{"backup":{"password":"private-value"}}`, `{"backup":{"schedule":"private-value"}}`, `{"backup":{"compression":"private-value"}}`, `{"backup":{"retention_days":0}}`, `{"auto_update":"private-value"}`,
		`{"backup":{"dump_timeout":"private-value"}}`,
		`{"backup":{"schedule":"CRON_TZ=UTC 0 4 * * *"}}`, `{"backup":{"schedule":"TZ=UTC 0 4 * * *"}}`, `{"backup":{"schedule":"@daily"}}`,
	} {
		_, err := DecodeDashboardOverrides([]byte(input))
		if err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("unsafe override accepted or echoed")
		}
	}
}
