package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/fivemanage/db-agent/internal/securefile"
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
	KeyFile         string `mapstructure:"key_file" yaml:"key_file"`
	KeyID           string `mapstructure:"key_id" yaml:"key_id,omitempty"`
	KeyAcknowledged bool   `mapstructure:"key_acknowledged" yaml:"key_acknowledged,omitempty"`
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
	v.SetConfigType("yaml")
	contents, err := securefile.Read(path, 1<<20)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("config file not found: %s", path)
		}

		return Config{}, errors.New("cannot read private config; check regular file, owner-only Unix permissions or System/Administrators-only Windows ACLs")
	}

	if err := v.ReadConfig(bytes.NewReader(contents)); err != nil {
		return Config{}, errors.New("cannot read config; check valid YAML")
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
	c, err := LoadDiagnostic(path)
	if err != nil {
		return Config{}, err
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
		return Config{}, errors.New("cannot read config; check valid YAML")
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

	if err := c.ValidateAgent(); err != nil {
		return Config{}, fmt.Errorf("invalid dashboard config: %w", err)
	}

	if err := c.Backup.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid dashboard config: %w", err)
	}

	return c, nil
}

func (c Config) Validate() error {
	if err := c.ValidateAgent(); err != nil {
		return err
	}

	if err := c.Database.Validate(); err != nil {
		return err
	}

	if err := c.Backup.Validate(); err != nil {
		return err
	}

	if strings.TrimSpace(c.Encryption.KeyFile) == "" {
		return errors.New("encryption key_file is required")
	}

	if !c.Encryption.KeyAcknowledged || !validKeyID(c.Encryption.KeyID) {
		return errors.New("recovery key must be saved and acknowledged; run db-agent configure --key")
	}

	return nil
}

func (c BackupConfig) Validate() error {
	if _, err := cron.ParseStandard(c.Schedule); err != nil {
		return errors.New("backup schedule must be a valid five-field cron expression")
	}

	if c.RetentionDays < 1 {
		return errors.New("backup retention_days must be at least 1")
	}

	if c.Compression != "zstd" && c.Compression != "gzip" {
		return errors.New("backup compression must be zstd or gzip")
	}

	return nil
}

func (c DatabaseConfig) Validate() error {
	if c.Driver != "mysql" && c.Driver != "mariadb" {
		return errors.New("database driver must be mysql or mariadb")
	}

	if strings.TrimSpace(c.Host) == "" {
		return errors.New("database host is required")
	}

	if c.Port < 1 || c.Port > 65535 {
		return errors.New("database port must be between 1 and 65535")
	}

	if strings.TrimSpace(c.User) == "" {
		return errors.New("database user is required")
	}

	if c.Password == "" {
		return errors.New("database password is required")
	}

	return nil
}

// LoadDiagnostic reads incomplete setup without suppressing independent checks.
// Parser errors can contain YAML scalar values, so they must not reach output.
func LoadDiagnostic(path string) (Config, error) {
	v := newViper()
	v.SetConfigType("yaml")
	contents, err := securefile.Read(ResolvePath(path), 1<<20)
	if err != nil {
		return Config{}, errors.New("cannot read private config; check regular file, owner-only Unix permissions or System/Administrators-only Windows ACLs")
	}

	if err := v.ReadConfig(bytes.NewReader(contents)); err != nil {
		return Config{}, errors.New("cannot read config; check valid YAML")
	}

	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return Config{}, errors.New("cannot decode config; check the YAML field types")
	}

	return c, nil
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
		return Config{}, errors.New("cannot decode config; check the YAML field types")
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
