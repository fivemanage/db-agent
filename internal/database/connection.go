package database

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/fivemanage/db-agent/internal/config"
)

// ParseConnection accepts common oxmysql URI and semicolon connection forms.
// Transport options are rejected rather than silently dropping TLS or sockets.
func ParseConnection(value string) (config.DatabaseConfig, error) {
	c := config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306}
	invalid := errors.New("invalid connection string; use mysql://user:password@127.0.0.1:3306/database or server=127.0.0.1;port=3306;database=game;userid=user;password=secret")
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "mysql://") {
		u, err := url.Parse(value)
		if err != nil || u.User == nil || u.Hostname() == "" || u.Fragment != "" || u.Opaque != "" {
			return config.DatabaseConfig{}, invalid
		}
		c.Host, c.User = u.Hostname(), u.User.Username()
		c.Password, _ = u.User.Password()
		if u.Port() != "" {
			c.Port, err = strconv.Atoi(u.Port())
			if err != nil {
				return config.DatabaseConfig{}, invalid
			}
		}
		if !strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path[1:], "/") {
			return config.DatabaseConfig{}, invalid
		}
		c.Databases = []string{strings.TrimPrefix(u.Path, "/")}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return config.DatabaseConfig{}, invalid
		}
		for key, values := range query {
			if key != "charset" || len(values) != 1 || values[0] != "utf8mb4" {
				return config.DatabaseConfig{}, errors.New("unsupported connection options; enter the local TCP connection manually (TLS/socket options are not imported)")
			}
		}
	} else {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ";") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			key, field, ok := strings.Cut(part, "=")
			if !ok {
				return config.DatabaseConfig{}, invalid
			}
			key = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), " ", ""))
			switch key {
			case "server", "host", "datasource":
				key = "host"
			case "userid", "user", "username", "uid":
				key = "user"
			case "password", "pwd":
				key = "password"
			case "database", "initialcatalog":
				key = "database"
			case "port":
			default:
				return config.DatabaseConfig{}, errors.New("unsupported connection-string field; enter the local TCP connection manually")
			}
			if seen[key] {
				return config.DatabaseConfig{}, invalid
			}
			seen[key] = true
			field = strings.TrimSpace(field)
			switch key {
			case "host":
				c.Host = field
			case "user":
				c.User = field
			case "password":
				c.Password = field
			case "database":
				c.Databases = []string{field}
			case "port":
				var err error
				c.Port, err = strconv.Atoi(field)
				if err != nil {
					return config.DatabaseConfig{}, invalid
				}
			}
		}
	}
	if err := ValidateLocalConnection(c); err != nil {
		return config.DatabaseConfig{}, err
	}
	return c, nil
}

func ValidateLocalConnection(c config.DatabaseConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	// Arbitrary hostnames could later resolve off-machine and leak admin secrets.
	if _, err := localTCPHost(c.Host); err != nil {
		return err
	}
	if len(c.Databases) == 0 {
		return errors.New("enter the application database name; no database is selected automatically")
	}
	for _, name := range c.Databases {
		if name == "" || systemDatabase(name) || strings.ContainsFunc(name, unicode.IsControl) {
			return errors.New("select a nonempty application database name without control characters; system databases are excluded")
		}
	}
	return nil
}
