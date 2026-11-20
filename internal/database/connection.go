package database

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/fivemanage/db-agent/internal/config"
)

type ParsedConnection struct {
	Database               config.DatabaseConfig
	IgnoredConnectionLimit bool
}

// ParseConnection accepts common oxmysql URI and semicolon connection forms.
// When TLS is requested, setup must supply a CA file before connecting.
func ParseConnection(value string) (ParsedConnection, error) {
	parsed := ParsedConnection{Database: config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306}}
	c := &parsed.Database
	var rawQuery string
	invalid := errors.New("invalid connection string; use mysql://user:password@127.0.0.1:3306/database or server=127.0.0.1;port=3306;database=game;userid=user;password=secret")
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "mysql://") {
		u, err := url.Parse(value)
		if err != nil || u.User == nil || u.Hostname() == "" || u.Fragment != "" || u.Opaque != "" {
			return ParsedConnection{}, invalid
		}
		c.Host, c.User = u.Hostname(), u.User.Username()
		c.Password, _ = u.User.Password()
		if u.Port() != "" {
			c.Port, err = strconv.Atoi(u.Port())
			if err != nil {
				return ParsedConnection{}, invalid
			}
		}
		if !strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path[1:], "/") {
			return ParsedConnection{}, invalid
		}
		c.Databases = []string{strings.TrimPrefix(u.Path, "/")}
		rawQuery = u.RawQuery
	} else {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ";") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			key, field, ok := strings.Cut(part, "=")
			if !ok {
				return ParsedConnection{}, invalid
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
				return ParsedConnection{}, errors.New("unsupported connection-string field; enter the local TCP connection manually")
			}
			if seen[key] {
				return ParsedConnection{}, invalid
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
					return ParsedConnection{}, invalid
				}
			}
		}
	}
	if err := ValidateLocalConnection(*c); err != nil {
		return ParsedConnection{}, err
	}
	if err := parseConnectionOptions(rawQuery, &parsed); err != nil {
		return ParsedConnection{}, err
	}

	return parsed, nil
}

func parseConnectionOptions(rawQuery string, parsed *ParsedConnection) error {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return errors.New("invalid connection string options; check the query syntax and percent encoding")
	}

	unsupported := errors.New("unsupported connection options; setup supports local TCP and only imports charset, connectionLimit, and ssl options")
	for key, values := range query {
		switch key {
		case "charset":
			if len(values) != 1 || values[0] != "utf8mb4" {
				return unsupported
			}
		case "connectionLimit":
			if len(values) != 1 {
				return errors.New("connectionLimit must not appear more than once")
			}
			// mysql2 uses JavaScript numbers and treats 0 as no pool limit.
			if _, err := strconv.ParseUint(values[0], 10, 53); err != nil {
				return errors.New("connectionLimit must be a nonnegative whole number within the supported range")
			}
			parsed.IgnoredConnectionLimit = true
		case "ssl":
			if len(values) != 1 || values[0] != "true" && values[0] != "false" {
				return errors.New("ssl must appear once with a value of true or false")
			}
			parsed.Database.TLS.Mode = config.TLSDisabled
			if values[0] == "true" {
				parsed.Database.TLS.Mode = config.TLSVerifyIdentity
			}
		default:
			return unsupported
		}
	}

	return nil
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
