package database

import (
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
)

func TestParseGameServerConnection(t *testing.T) {
	for _, value := range []string{
		"mysql://backup:s%40fe%3Apassword@127.0.0.1/game_db?charset=utf8mb4",
		"server=127.0.0.1;database=game_db;User ID=backup;password=s@fe:password;",
	} {
		parsed, err := ParseConnection(value)
		c := parsed.Database
		if err != nil || parsed.IgnoredConnectionLimit || c.Host != "127.0.0.1" || c.Port != 3306 || c.User != "backup" || c.Password != "s@fe:password" || len(c.Databases) != 1 || c.Databases[0] != "game_db" {
			t.Fatal("connection fields were not imported correctly")
		}
	}
	parsed, err := ParseConnection("mysql://backup:secret@[::1]:3307/game")
	if err != nil || parsed.IgnoredConnectionLimit || parsed.Database.Host != "::1" || parsed.Database.Port != 3307 {
		t.Fatal("IPv6 loopback connection failed")
	}
}

func TestParseConnectionIgnoresApplicationPoolLimit(t *testing.T) {
	for _, test := range []struct {
		name, query string
	}{
		{"reported string", "charset=utf8mb4&connectionLimit=50"},
		{"no pool limit", "connectionLimit=0"},
		{"percent-encoded value", "connectionLimit=%35%30&charset=utf8mb4"},
		{"largest safe integer", "connectionLimit=9007199254740991"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := ParseConnection("mysql://backup:s%40fe%3Apassword@localhost:3307/game_db?" + test.query)

			c := parsed.Database
			if err != nil {
				t.Fatalf("pool option was rejected: %v", err)
			}
			if !parsed.IgnoredConnectionLimit || c.Host != "localhost" || c.Port != 3307 || c.User != "backup" || c.Password != "s@fe:password" || len(c.Databases) != 1 || c.Databases[0] != "game_db" {
				t.Fatal("pool option was not reported as ignored or connection fields changed")
			}
		})
	}
}

func TestParseConnectionImportsSSLMode(t *testing.T) {
	for _, test := range []struct {
		query, mode string
	}{
		{"charset=utf8mb4&connectionLimit=50", ""},
		{"charset=utf8mb4&connectionLimit=50&ssl=true", config.TLSVerifyIdentity},
		{"ssl=false", config.TLSDisabled},
	} {
		t.Run(test.query, func(t *testing.T) {
			parsed, err := ParseConnection("mysql://backup:private-password@localhost/game?" + test.query)
			if err != nil || parsed.Database.TLS.Mode != test.mode || parsed.Database.TLS.CAFile != "" {
				t.Fatalf("SSL setting was not imported as expected: %v", err)
			}
		})
	}
}

func TestParseConnectionRejectsUnsupportedOrMalformedOptions(t *testing.T) {
	for _, test := range []struct {
		name, query string
	}{
		{"unrecognized TLS mode", "connectionLimit=50&sslMode=required"},
		{"unrecognized TLS value", "ssl=private-option"},
		{"TLS object", "ssl=%7B%22rejectUnauthorized%22%3Afalse%7D"},
		{"duplicate TLS mode", "ssl=true&ssl=false"},
		{"missing TLS value", "ssl"},
		{"socket path", "connectionLimit=50&socketPath=/tmp/mysql.sock"},
		{"other connection behavior", "connectionLimit=50&multipleStatements=true"},
		{"unsupported charset", "connectionLimit=50&charset=latin1"},
		{"duplicate charset", "charset=utf8mb4&charset=utf8mb4&connectionLimit=50"},
		{"duplicate limit", "connectionLimit=50&connectionLimit=50"},
		{"encoded duplicate key", "connectionLimit=50&connection%4Cimit=51"},
		{"missing value", "connectionLimit"},
		{"empty value", "connectionLimit="},
		{"negative value", "connectionLimit=-1"},
		{"fractional value", "connectionLimit=1.5"},
		{"overflow", "connectionLimit=9007199254740992"},
		{"bad percent encoding", "connectionLimit=50&private-option=%gg"},
		{"invalid separator", "connectionLimit=50;socketPath=private-option"},
		{"unrecognized spelling", "connectionlimit=50"},
		{"unrecognized key", "connectionLimit=50&private-option=1"},
		{"unrecognized value", "connectionLimit=private-option"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := ParseConnection("mysql://private-user:private-password@127.0.0.1/game?" + test.query)

			if err == nil {
				t.Fatal("invalid connection options were accepted")
			}
			if parsed.IgnoredConnectionLimit || parsed.Database.Password != "" {
				t.Fatal("failed import returned connection data")
			}
			for _, secret := range []string{"private-user", "private-password", "private-option"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("connection error exposed pasted input")
				}
			}
		})
	}
}

func TestParseConnectionRejectsUnsafeOrAmbiguousInputsWithoutSecrets(t *testing.T) {
	for _, value := range []string{
		"mysql://backup:private-secret@remote.example/game?charset=utf8mb4&connectionLimit=50",
		"mysql://backup:private-secret@remote.example/game?ssl=true",
		"mysql://backup:private-secret@127.0.0.1/",
		"mysql://backup:private-secret@127.0.0.1/mysql",
		"mysql://backup:private-secret@127.0.0.1/game/other",
		"mysql://backup:private-secret@127.0.0.1/game#fragment",
		"mysql://backup:private-secret@127.0.0.1:0/game",
		"mysql://backup:private-secret@127.0.0.1/game%0Aevil",
		"mysql://backup:private-secret@127.0.0.1:%bad/game",
		"server=localhost;database=game;user=backup;uid=root;password=private-secret",
		"server=localhost;database=game;user=backup;password=private-secret;sslmode=required",
		"server=localhost;user=backup;password=private-secret",
		"private-secret",
	} {
		_, err := ParseConnection(value)
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid connection accepted or secret leaked")
		}
	}
}
