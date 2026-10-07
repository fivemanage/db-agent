package database

import (
	"strings"
	"testing"
)

func TestParseGameServerConnection(t *testing.T) {
	for _, value := range []string{
		"mysql://backup:s%40fe%3Apassword@127.0.0.1/game_db?charset=utf8mb4",
		"server=127.0.0.1;database=game_db;User ID=backup;password=s@fe:password;",
	} {
		c, err := ParseConnection(value)
		if err != nil || c.Host != "127.0.0.1" || c.Port != 3306 || c.User != "backup" || c.Password != "s@fe:password" || len(c.Databases) != 1 || c.Databases[0] != "game_db" {
			t.Fatal("connection fields were not imported correctly")
		}
	}
	c, err := ParseConnection("mysql://backup:secret@[::1]:3307/game")
	if err != nil || c.Host != "::1" || c.Port != 3307 {
		t.Fatal("IPv6 loopback connection failed")
	}
}

func TestParseConnectionRejectsUnsafeOrAmbiguousInputsWithoutSecrets(t *testing.T) {
	for _, value := range []string{
		"mysql://backup:private-secret@remote.example/game",
		"mysql://backup:private-secret@127.0.0.1/",
		"mysql://backup:private-secret@127.0.0.1/mysql",
		"mysql://backup:private-secret@127.0.0.1/game/other",
		"mysql://backup:private-secret@127.0.0.1/game#fragment",
		"mysql://backup:private-secret@127.0.0.1/game?ssl=true",
		"mysql://backup:private-secret@127.0.0.1/game?charset=utf8mb4&charset=utf8mb4",
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
