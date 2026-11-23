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

func TestParseWindowsSearchPath(t *testing.T) {
	raw := ` ; .\relative ; C:drive-relative ; \\server\share\bin ; "%ProgramFiles%\Unexpanded" ; "C:\Program Files\MariaDB 11.4\bin\" ; c:\program files\mariadb 11.4\bin ; C:\Program Files\MySQL\MySQL Server 8.4\bin `

	got := parseWindowsSearchPath(raw)
	want := []string{
		`C:\Program Files\MariaDB 11.4\bin`,
		`C:\Program Files\MySQL\MySQL Server 8.4\bin`,
	}

	if len(got) != len(want) {
		t.Fatalf("got=%q want=%q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestLookupFromSearchPathPreservesProcessOrderAndSkipsUntrustedMachineDirs(t *testing.T) {
	t.Run("prefers existing process PATH", func(t *testing.T) {
		machineCalled := false
		got, err := lookupFromSearchPath(
			"mariadb-dump",
			func(string) (string, error) { return `C:\Trusted\bin\mariadb-dump.exe`, nil },
			func() (string, error) {
				machineCalled = true
				return `C:\Program Files\MariaDB 11.4\bin`, nil
			},
			func(string) bool { return true },
			func(dir, name string) (string, bool) { return dir + `\` + name + ".exe", true },
		)
		if err != nil || got != `C:\Trusted\bin\mariadb-dump.exe` || machineCalled {
			t.Fatalf("got=%q err=%v machineCalled=%v", got, err, machineCalled)
		}
	})

	t.Run("discovers trusted machine PATH dir while skipping user-writable dirs", func(t *testing.T) {
		raw := `C:\Users\alice\bin;\\fileserver\tools;C:\Program Files\MariaDB 11.4\bin;C:\Program Files\MariaDB 11.8\bin`
		got, err := lookupFromSearchPath(
			"mariadb-dump",
			func(string) (string, error) { return "", errors.New("not found") },
			func() (string, error) { return raw, nil },
			func(dir string) bool { return strings.HasPrefix(dir, `C:\Program Files\`) },
			func(dir, name string) (string, bool) {
				return dir + `\` + name + ".exe", true
			},
		)
		if err != nil || got != `C:\Program Files\MariaDB 11.4\bin\mariadb-dump.exe` {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("missing client advises system PATH and service restart", func(t *testing.T) {
		_, check := detectTools(
			context.Background(),
			"mariadb",
			"11.4.8",
			func(string) (string, error) { return "", errors.New("not found") },
			func(context.Context, string) (string, error) { return "", errors.New("unexpected") },
		)
		if check.Passed || !strings.Contains(check.Message, "restart the db-agent service") {
			t.Fatalf("expected restart guidance, got: %+v", check)
		}
	})
}
