package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAgentBeforeDatabaseSetup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("agent_credential: dac_test\napi_url: https://api.example.com\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AgentCredential != "dac_test" || !settings.AutoUpdate {
		t.Fatalf("LoadAgent() = %+v", settings)
	}
}

func TestLoadUpdateBeforeEnrollment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("update_url: https://releases.example.com/{os}-{arch}.json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadUpdate(path)
	if err != nil || settings.UpdateURL == "" {
		t.Fatalf("LoadUpdate() = %+v, %v", settings, err)
	}
}

func TestLoadAgentRejectsInsecureUpdateURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	input := "agent_credential: dac_test\napi_url: https://api.example.com\nupdate_url: http://updates.example.com/{os}-{arch}.json\n"
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(path); err == nil || !strings.Contains(err.Error(), "update_url must use HTTPS") {
		t.Fatalf("LoadAgent() error = %v, want HTTPS update URL error", err)
	}
}

const minimalConfig = `
agent_credential: dac_test
api_url: https://api.example.com
database:
  user: backup
  password: secret
encryption:
  key_file: /etc/db-agent/encryption.key
`

func TestLoadReaderAppliesDefaults(t *testing.T) {
	config, err := LoadReader(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatalf("LoadReader() error = %v", err)
	}

	if config.Database.Driver != "mysql" {
		t.Errorf("Database.Driver = %q, want mysql", config.Database.Driver)
	}
	if config.Database.Host != "127.0.0.1" {
		t.Errorf("Database.Host = %q, want 127.0.0.1", config.Database.Host)
	}
	if config.Database.Port != 3306 {
		t.Errorf("Database.Port = %d, want 3306", config.Database.Port)
	}
	if config.Backup.Schedule != "0 4 * * *" {
		t.Errorf("Backup.Schedule = %q, want 0 4 * * *", config.Backup.Schedule)
	}
	if config.Backup.RetentionDays != 7 {
		t.Errorf("Backup.RetentionDays = %d, want 7", config.Backup.RetentionDays)
	}
	if config.Backup.Compression != "zstd" {
		t.Errorf("Backup.Compression = %q, want zstd", config.Backup.Compression)
	}
	if !config.Backup.ThrottleIO {
		t.Error("Backup.ThrottleIO = false, want true")
	}
	if !config.AutoUpdate {
		t.Error("AutoUpdate = false, want true")
	}
}

func TestLoadReaderValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{name: "missing credential", config: strings.Replace(minimalConfig, "agent_credential: dac_test\n", "", 1), wantErr: "agent_credential is required"},
		{name: "invalid API URL", config: strings.Replace(minimalConfig, "https://api.example.com", "://bad", 1), wantErr: "api_url must be a valid HTTP or HTTPS URL"},
		{name: "invalid driver", config: strings.Replace(minimalConfig, "database:\n", "database:\n  driver: postgres\n", 1), wantErr: "database driver must be mysql or mariadb"},
		{name: "invalid schedule", config: minimalConfig + "backup:\n  schedule: sometimes\n", wantErr: "backup schedule must be a valid five-field cron expression"},
		{name: "invalid compression", config: minimalConfig + "backup:\n  compression: zip\n", wantErr: "backup compression must be zstd or gzip"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadReader(strings.NewReader(test.config))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("LoadReader() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadReaderPreservesExplicitFalseValues(t *testing.T) {
	input := minimalConfig + `
backup:
  throttle_io: false
auto_update: false
`

	config, err := LoadReader(strings.NewReader(input))
	if err != nil {
		t.Fatalf("LoadReader() error = %v", err)
	}
	if config.Backup.ThrottleIO {
		t.Error("Backup.ThrottleIO = true, want false")
	}
	if config.AutoUpdate {
		t.Error("AutoUpdate = true, want false")
	}
}

func TestDefaultPathForOS(t *testing.T) {
	tests := []struct {
		name        string
		goos        string
		programData string
		want        string
	}{
		{name: "Windows default", goos: "windows", want: `C:\ProgramData\db-agent\config.yaml`},
		{name: "Windows ProgramData", goos: "windows", programData: `D:\ProgramData`, want: `D:\ProgramData\db-agent\config.yaml`},
		{name: "Linux", goos: "linux", want: "/etc/db-agent/config.yaml"},
		{name: "macOS", goos: "darwin", want: "/etc/db-agent/config.yaml"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DefaultPathForOS(test.goos, test.programData); got != test.want {
				t.Errorf("DefaultPathForOS() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolvePathPrecedence(t *testing.T) {
	t.Setenv(PathEnvironmentVariable, "/from/environment.yaml")

	if got := ResolvePath("/from/flag.yaml"); got != "/from/flag.yaml" {
		t.Errorf("ResolvePath(flag) = %q, want /from/flag.yaml", got)
	}
	if got := ResolvePath(""); got != "/from/environment.yaml" {
		t.Errorf("ResolvePath(environment) = %q, want /from/environment.yaml", got)
	}
}

func TestLoadReportsMissingConfig(t *testing.T) {
	_, err := Load("/does/not/exist")
	if err == nil || err.Error() != "config file not found: /does/not/exist" {
		t.Fatalf("Load() error = %v, want missing config error", err)
	}
}

func TestWithDashboardOverrides(t *testing.T) {
	local, err := LoadReader(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatalf("LoadReader() error = %v", err)
	}

	schedule := "0 2 * * *"
	retentionDays := 14
	compression := "gzip"
	throttleIO := false
	autoUpdate := false
	effective, err := local.WithDashboardOverrides(DashboardOverrides{
		Backup: BackupOverrides{
			Schedule:      &schedule,
			RetentionDays: &retentionDays,
			Compression:   &compression,
			ThrottleIO:    &throttleIO,
		},
		AutoUpdate: &autoUpdate,
	})
	if err != nil {
		t.Fatalf("WithDashboardOverrides() error = %v", err)
	}

	if effective.Backup.Schedule != schedule || effective.Backup.RetentionDays != retentionDays || effective.Backup.Compression != compression || effective.Backup.ThrottleIO != throttleIO || effective.AutoUpdate != autoUpdate {
		t.Errorf("WithDashboardOverrides() did not apply all overrides: %+v", effective)
	}
	if effective.AgentCredential != local.AgentCredential || effective.Database.Password != local.Database.Password || effective.Encryption.KeyFile != local.Encryption.KeyFile {
		t.Error("WithDashboardOverrides() changed a local-only setting")
	}
}

func TestWithDashboardOverridesRejectsInvalidConfig(t *testing.T) {
	local, err := LoadReader(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatalf("LoadReader() error = %v", err)
	}

	compression := "zip"
	_, err = local.WithDashboardOverrides(DashboardOverrides{
		Backup: BackupOverrides{Compression: &compression},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid dashboard config: backup compression must be zstd or gzip") {
		t.Fatalf("WithDashboardOverrides() error = %v", err)
	}
}
