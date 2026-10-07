package database

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDetectTools(t *testing.T) {
	for _, test := range []struct {
		name, driver, server string
		tools                map[string]string
		wantTool, message    string
		passed               bool
	}{
		{"mysql", "mysql", "8.4.2", map[string]string{"mysqldump": "mysqldump Ver 8.4.2 for Linux on x86_64"}, "mysqldump", "Will use", true},
		{"mariadb", "mariadb", "11.4.8", map[string]string{"mariadb-dump": "mariadb-dump Ver 10.19 Distrib 11.4.8-MariaDB, for Linux"}, "mariadb-dump", "Will use", true},
		{"modern mariadb", "mariadb", "11.4.13", map[string]string{"mariadb-dump": "mariadb-dump from 11.4.13-MariaDB, client 10.19 for debian-linux-gnu (x86_64)"}, "mariadb-dump", "Will use", true},
		{"missing", "mysql", "8.4.2", nil, "", "mysql-client/mysql-community-client", false},
		{"old client", "mysql", "8.4.2", map[string]string{"mysqldump": "mysqldump Ver 5.7.44"}, "", "unsupported", false},
		{"old minor", "mysql", "8.4.2", map[string]string{"mysqldump": "mysqldump Ver 8.0.42"}, "", "unsupported", false},
		{"wrong family", "mysql", "8.4.2", map[string]string{"mysqldump": "mysqldump Ver 10.19 Distrib 11.4.8-MariaDB"}, "", "unsupported", false},
		{"unparseable", "mysql", "8.4.2", map[string]string{"mysqldump": "secret untrusted output"}, "", "unsupported", false},
		{"physical only", "mariadb", "11.4.8", map[string]string{"mariabackup": "", "xtrabackup": ""}, "", "Physical tools detected", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(name string) (string, error) {
				if _, ok := test.tools[name]; ok {
					return name, nil
				}

				return "", errors.New("not found")
			}
			run := func(_ context.Context, name string) (string, error) { return test.tools[name], nil }

			tool, check := detectTools(context.Background(), test.driver, test.server, lookup, run)
			if check.Passed != test.passed || tool.Name != test.wantTool || !strings.Contains(check.Message, test.message) {
				t.Fatalf("tool=%+v check=%+v", tool, check)
			}

			if strings.Contains(check.Message, "secret untrusted") {
				t.Fatal("raw version output leaked")
			}
		})
	}
}

func TestToolFailureIsRedacted(t *testing.T) {
	_, check := detectTools(context.Background(), "mysql", "8.4.2", func(name string) (string, error) { return name, nil }, func(context.Context, string) (string, error) { return "secret", errors.New("secret") })
	if check.Passed || strings.Contains(check.Message, "secret") {
		t.Fatalf("check: %+v", check)
	}
}

func TestToolOutputIsBounded(t *testing.T) {
	var output cappedOutput
	data := strings.Repeat("x", 8192)

	n, err := output.Write([]byte(data))
	if err != nil || n != len(data) || output.Len() != 4096 {
		t.Fatalf("write=%d len=%d err=%v", n, output.Len(), err)
	}

	var streamed cappedOutput
	if _, err := io.Copy(&streamed, struct{ io.Reader }{strings.NewReader(data)}); err != nil || streamed.Len() != 4096 {
		t.Fatalf("streamed output length=%d err=%v", streamed.Len(), err)
	}
}
