package config

import (
	"bytes"
	"errors"
	"net"
	"regexp"
	"slices"
	"strings"

	"github.com/fivemanage/db-agent/internal/securefile"
	"gopkg.in/yaml.v3"
)

var ErrRestoreAuthorization = errors.New("local restore access is not authorized for this database server; run db-agent configure --restore-access")

var restoreUserPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// RestoreConfig authorizes only the local provisioning connection, never dump
// execution. An importer must use a separate destination-confined principal.
type RestoreConfig struct {
	Authorized bool   `mapstructure:"authorized" yaml:"authorized" json:"-"`
	Driver     string `mapstructure:"driver" yaml:"driver" json:"-"`
	Host       string `mapstructure:"host" yaml:"host" json:"-"`
	Port       int    `mapstructure:"port" yaml:"port" json:"-"`
	User       string `mapstructure:"user" yaml:"user" json:"-"`
	Password   string `mapstructure:"password" yaml:"password" json:"-"`
}

func (r RestoreConfig) String() string   { return "private local restore authorization" }
func (r RestoreConfig) GoString() string { return r.String() }

func restoreHost(host string) string {
	if strings.EqualFold(host, "localhost") {
		return "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.Equal(net.ParseIP("127.0.0.1")) || ip.Equal(net.ParseIP("::1"))) {
		return ip.String()
	}
	return ""
}

func (r RestoreConfig) ValidateFor(database DatabaseConfig) error {
	if !r.Authorized || !restoreUserPattern.MatchString(r.User) || r.Password == "" || strings.ContainsRune(r.Password, 0) ||
		strings.EqualFold(r.User, "root") || strings.EqualFold(r.User, database.User) || database.User == "" ||
		(r.Driver != "mysql" && r.Driver != "mariadb") || r.Driver != database.Driver ||
		r.Port < 1 || r.Port > 65535 || r.Port != database.Port || restoreHost(r.Host) == "" || restoreHost(r.Host) != restoreHost(database.Host) || len(database.Databases) == 0 {
		return ErrRestoreAuthorization
	}
	for _, name := range database.Databases {
		switch strings.ToLower(name) {
		case "", "mysql", "sys", "information_schema", "performance_schema":
			return ErrRestoreAuthorization
		}
		if strings.HasPrefix(strings.ToLower(name), "fm_restore_") {
			return ErrRestoreAuthorization
		}
	}
	return nil
}

// SaveRestoreAccess changes only local authorization. Recheck the database
// snapshot inside the existing atomic update to avoid approving another server
// or backup identity after a concurrent configure operation.
func SaveRestoreAccess(path string, expected DatabaseConfig, access RestoreConfig) error {
	if err := access.ValidateFor(expected); err != nil {
		return err
	}
	if _, err := securefile.Read(ResolvePath(path), 1<<20); err != nil {
		return errors.New("cannot read private config for restore authorization; existing settings are preserved")
	}
	return updateDocument(path, func(root *yaml.Node) error {
		data, err := yaml.Marshal(root)
		if err != nil {
			return errors.New("cannot validate private config for restore authorization")
		}
		v := newViper()
		v.SetConfigType("yaml")
		var current Config
		if v.ReadConfig(bytes.NewReader(data)) != nil || v.Unmarshal(&current) != nil {
			return errors.New("cannot validate private config for restore authorization")
		}
		a, b := current.Database, expected
		if a.Driver != b.Driver || a.Host != b.Host || a.Port != b.Port || a.User != b.User || a.Password != b.Password || !slices.Equal(a.Databases, b.Databases) {
			return errors.New("database settings changed during restore setup; rerun configure --restore-access; existing settings are preserved")
		}
		var section *yaml.Node
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "restore" {
				section = root.Content[i+1]
			}
		}
		if section == nil {
			section = &yaml.Node{Kind: yaml.MappingNode}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "restore"}, section)
		}
		if section.Kind != yaml.MappingNode {
			return errors.New("restore settings must be a valid private YAML mapping")
		}
		for _, field := range []struct {
			name  string
			value any
		}{
			{"authorized", access.Authorized}, {"driver", access.Driver}, {"host", access.Host}, {"port", access.Port},
			{"user", access.User}, {"password", access.Password},
		} {
			if err := setYAML(section, field.name, field.value); err != nil {
				return err
			}
		}
		return nil
	})
}
