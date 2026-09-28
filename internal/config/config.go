package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
)

const PathEnvironmentVariable = "DB_AGENT_CONFIG"

type Config struct {
	AgentCredential string           `mapstructure:"agent_credential" yaml:"agent_credential"`
	APIURL          string           `mapstructure:"api_url" yaml:"api_url"`
	UpdateURL       string           `mapstructure:"update_url" yaml:"update_url"`
	Database        DatabaseConfig   `mapstructure:"database" yaml:"database"`
	Backup          BackupConfig     `mapstructure:"backup" yaml:"backup"`
	Encryption      EncryptionConfig `mapstructure:"encryption" yaml:"encryption"`
	AutoUpdate      bool             `mapstructure:"auto_update" yaml:"auto_update"`
}

type DatabaseConfig struct {
	Driver    string   `mapstructure:"driver" yaml:"driver"`
	Host      string   `mapstructure:"host" yaml:"host"`
	Port      int      `mapstructure:"port" yaml:"port"`
	User      string   `mapstructure:"user" yaml:"user"`
	Password  string   `mapstructure:"password" yaml:"password"`
	Databases []string `mapstructure:"databases" yaml:"databases"`
}

type BackupConfig struct {
	Schedule      string `mapstructure:"schedule" yaml:"schedule"`
	RetentionDays int    `mapstructure:"retention_days" yaml:"retention_days"`
	Compression   string `mapstructure:"compression" yaml:"compression"`
	ThrottleIO    bool   `mapstructure:"throttle_io" yaml:"throttle_io"`
}

type EncryptionConfig struct {
	KeyFile string `mapstructure:"key_file" yaml:"key_file"`
}

// DashboardOverrides deliberately excludes secrets and machine-specific settings.
type DashboardOverrides struct {
	Backup     BackupOverrides `json:"backup"`
	AutoUpdate *bool           `json:"auto_update,omitempty"`
}

type BackupOverrides struct {
	Schedule      *string `json:"schedule,omitempty"`
	RetentionDays *int    `json:"retention_days,omitempty"`
	Compression   *string `json:"compression,omitempty"`
	ThrottleIO    *bool   `json:"throttle_io,omitempty"`
}

func Load(path string) (Config, error) {
	path = ResolvePath(path)

	v := newViper()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	if err := v.ReadInConfig(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("config file not found: %s", path)
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	return decode(v)
}

// LoadAgent only requires the control-plane settings needed for enrollment,
// heartbeat, and updates. Database settings can be completed before backups.
func LoadAgent(path string) (Config, error) {
	c, err := LoadUpdate(path)
	if err != nil {
		return Config{}, err
	}
	if err := c.ValidateAgent(); err != nil {
		return Config{}, fmt.Errorf("invalid agent config: %w", err)
	}
	return c, nil
}

// LoadUpdate lets release checks run before agent enrollment or DB setup.
func LoadUpdate(path string) (Config, error) {
	v := newViper()
	v.SetConfigFile(ResolvePath(path))
	v.SetConfigType("yaml")
	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("read agent config: %w", err)
	}
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return Config{}, fmt.Errorf("decode agent config: %w", err)
	}
	if err := c.ValidateUpdate(); err != nil {
		return Config{}, fmt.Errorf("invalid update config: %w", err)
	}
	return c, nil
}

func LoadReader(reader io.Reader) (Config, error) {
	v := newViper()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(reader); err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	return decode(v)
}

func ResolvePath(override string) string {
	if override != "" {
		return override
	}
	if path := os.Getenv(PathEnvironmentVariable); path != "" {
		return path
	}

	return DefaultPathForOS(runtime.GOOS, os.Getenv("ProgramData"))
}

func DefaultPathForOS(goos, programData string) string {
	if goos == "windows" {
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return strings.TrimRight(programData, `\/`) + `\db-agent\config.yaml`
	}

	return "/etc/db-agent/config.yaml"
}

func (c Config) WithDashboardOverrides(overrides DashboardOverrides) (Config, error) {
	if overrides.Backup.Schedule != nil {
		c.Backup.Schedule = *overrides.Backup.Schedule
	}
	if overrides.Backup.RetentionDays != nil {
		c.Backup.RetentionDays = *overrides.Backup.RetentionDays
	}
	if overrides.Backup.Compression != nil {
		c.Backup.Compression = *overrides.Backup.Compression
	}
	if overrides.Backup.ThrottleIO != nil {
		c.Backup.ThrottleIO = *overrides.Backup.ThrottleIO
	}
	if overrides.AutoUpdate != nil {
		c.AutoUpdate = *overrides.AutoUpdate
	}

	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid dashboard config: %w", err)
	}
	return c, nil
}

func (c Config) Validate() error {
	if err := c.ValidateAgent(); err != nil {
		return err
	}
	if c.Database.Driver != "mysql" && c.Database.Driver != "mariadb" {
		return errors.New("database driver must be mysql or mariadb")
	}
	if strings.TrimSpace(c.Database.Host) == "" {
		return errors.New("database host is required")
	}
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		return errors.New("database port must be between 1 and 65535")
	}
	if strings.TrimSpace(c.Database.User) == "" {
		return errors.New("database user is required")
	}
	if c.Database.Password == "" {
		return errors.New("database password is required")
	}
	if _, err := cron.ParseStandard(c.Backup.Schedule); err != nil {
		return errors.New("backup schedule must be a valid five-field cron expression")
	}
	if c.Backup.RetentionDays < 1 {
		return errors.New("backup retention_days must be at least 1")
	}
	if c.Backup.Compression != "zstd" && c.Backup.Compression != "gzip" {
		return errors.New("backup compression must be zstd or gzip")
	}
	if strings.TrimSpace(c.Encryption.KeyFile) == "" {
		return errors.New("encryption key_file is required")
	}

	return nil
}

func (c Config) ValidateAgent() error {
	if strings.TrimSpace(c.AgentCredential) == "" {
		return errors.New("agent_credential is required")
	}
	return c.ValidateUpdate()
}

func (c Config) ValidateUpdate() error {
	if err := validateAPIURL(c.APIURL); err != nil {
		return err
	}
	if c.UpdateURL != "" {
		value := strings.ReplaceAll(strings.ReplaceAll(c.UpdateURL, "{os}", "linux"), "{arch}", "amd64")
		if err := validateAPIURL(value); err != nil {
			return fmt.Errorf("update_url: %w", err)
		}
		parsed, _ := url.Parse(value)
		if parsed.Scheme != "https" {
			return errors.New("update_url must use HTTPS")
		}
	}
	return nil
}

func newViper() *viper.Viper {
	v := viper.New()
	v.SetDefault("database.driver", "mysql")
	v.SetDefault("database.host", "127.0.0.1")
	v.SetDefault("database.port", 3306)
	v.SetDefault("backup.schedule", "0 4 * * *")
	v.SetDefault("backup.retention_days", 7)
	v.SetDefault("backup.compression", "zstd")
	v.SetDefault("backup.throttle_io", true)
	v.SetDefault("auto_update", true)

	// this should be a build variable
	v.SetDefault("api_url", "https://api.example.com")
	return v
}

func decode(v *viper.Viper) (Config, error) {
	var config Config
	if err := v.Unmarshal(&config); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}

	return config, nil
}

func validateAPIURL(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("api_url must be a valid HTTP or HTTPS URL")
	}
	return nil
}
