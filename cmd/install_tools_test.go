package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
)

func TestEnsureDumpToolSucceedsImmediatelyWhenAlreadyOnPath(t *testing.T) {
	var output bytes.Buffer
	setup := dumpToolSetup{
		inspect: func(context.Context, config.DatabaseConfig) database.Report {
			return database.Report{Driver: "mariadb", Version: "11.4.2"}
		},
		detect: func(context.Context, string, string) (database.Tool, database.Check) {
			return database.Tool{Name: "mariadb-dump", Version: "11.4.8"}, database.Check{Passed: true}
		},
		discover: func(context.Context, string, string) []database.DiscoveredTool {
			t.Fatal("should not scan off-PATH when tool is already found")
			return nil
		},
		planInstall: func(string) (database.ToolInstallPlan, bool) { return database.ToolInstallPlan{}, false },
	}

	ensureDumpTool(context.Background(), bufio.NewReader(strings.NewReader("")), &output, config.DatabaseConfig{}, setup)

	if !strings.Contains(output.String(), "+ Found mariadb-dump 11.4.8") {
		t.Fatalf("unexpected output: %s", output.String())
	}
}

func TestEnsureDumpToolAddsTrustedOffPathDirToSystemPath(t *testing.T) {
	var output bytes.Buffer
	addedDir := ""
	detectCalls := 0
	setup := dumpToolSetup{
		inspect: func(context.Context, config.DatabaseConfig) database.Report {
			return database.Report{Driver: "mariadb", Version: "11.4.2"}
		},
		detect: func(context.Context, string, string) (database.Tool, database.Check) {
			detectCalls++
			if detectCalls == 1 {
				return database.Tool{}, database.Check{Passed: false}
			}
			return database.Tool{Name: "mariadb-dump", Version: "11.4.8"}, database.Check{Passed: true}
		},
		discover: func(context.Context, string, string) []database.DiscoveredTool {
			return []database.DiscoveredTool{{
				Tool:    database.Tool{Name: "mariadb-dump", Path: `C:\Program Files\MariaDB 11.4\bin\mariadb-dump.exe`, Version: "11.4.8"},
				Dir:     `C:\Program Files\MariaDB 11.4\bin`,
				Trusted: true,
			}}
		},
		addToPath: func(dir string) error {
			addedDir = dir
			return nil
		},
		planInstall: func(string) (database.ToolInstallPlan, bool) { return database.ToolInstallPlan{}, false },
		isWindows:   true,
	}

	ensureDumpTool(context.Background(), bufio.NewReader(strings.NewReader("y\n")), &output, config.DatabaseConfig{}, setup)

	if addedDir != `C:\Program Files\MariaDB 11.4\bin` {
		t.Fatalf("expected PATH add, got %q", addedDir)
	}
	if !strings.Contains(output.String(), "Added C:\\Program Files\\MariaDB 11.4\\bin to System PATH and verified mariadb-dump 11.4.8") {
		t.Fatalf("missing verification line: %s", output.String())
	}
}

func TestEnsureDumpToolInformsUserWhenSystemPathWriteFails(t *testing.T) {
	var output bytes.Buffer
	setup := dumpToolSetup{
		inspect: func(context.Context, config.DatabaseConfig) database.Report {
			return database.Report{Driver: "mysql", Version: "8.4.0"}
		},
		detect: func(context.Context, string, string) (database.Tool, database.Check) {
			return database.Tool{}, database.Check{Passed: false}
		},
		discover: func(context.Context, string, string) []database.DiscoveredTool {
			return []database.DiscoveredTool{{
				Tool:    database.Tool{Name: "mysqldump", Path: `C:\Program Files\MySQL\MySQL Server 8.4\bin\mysqldump.exe`, Version: "8.4.2"},
				Dir:     `C:\Program Files\MySQL\MySQL Server 8.4\bin`,
				Trusted: true,
			}}
		},
		addToPath: func(string) error {
			return errors.New("access denied")
		},
		planInstall: func(string) (database.ToolInstallPlan, bool) { return database.ToolInstallPlan{}, false },
		isWindows:   true,
	}

	ensureDumpTool(context.Background(), bufio.NewReader(strings.NewReader("y\nn\n")), &output, config.DatabaseConfig{}, setup)

	text := output.String()
	for _, want := range []string{"Couldn't add C:\\Program Files\\MySQL\\MySQL Server 8.4\\bin to System PATH automatically", "access denied", "Add C:\\Program Files\\MySQL\\MySQL Server 8.4\\bin to your System PATH manually"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
}

func TestEnsureDumpToolExplainsUntrustedDirectory(t *testing.T) {
	var output bytes.Buffer
	setup := dumpToolSetup{
		inspect: func(context.Context, config.DatabaseConfig) database.Report {
			return database.Report{Driver: "mariadb", Version: "10.4.32"}
		},
		detect: func(context.Context, string, string) (database.Tool, database.Check) {
			return database.Tool{}, database.Check{Passed: false}
		},
		discover: func(context.Context, string, string) []database.DiscoveredTool {
			return []database.DiscoveredTool{{
				Tool:    database.Tool{Name: "mysqldump", Path: `C:\xampp\mysql\bin\mysqldump.exe`},
				Dir:     `C:\xampp\mysql\bin`,
				Trusted: false,
			}}
		},
		addToPath: func(string) error {
			t.Fatal("must never add untrusted directory to System PATH")
			return nil
		},
		planInstall: func(string) (database.ToolInstallPlan, bool) { return database.ToolInstallPlan{}, false },
		isWindows:   true,
	}

	ensureDumpTool(context.Background(), bufio.NewReader(strings.NewReader("n\n")), &output, config.DatabaseConfig{}, setup)

	if !strings.Contains(output.String(), "writable by non-administrators") {
		t.Fatalf("missing untrusted directory explanation: %s", output.String())
	}
}

func TestEnsureDumpToolInstallsLinuxPackageWithConfirmation(t *testing.T) {
	var output bytes.Buffer
	installed := false
	detectCalls := 0
	setup := dumpToolSetup{
		inspect: func(context.Context, config.DatabaseConfig) database.Report {
			return database.Report{Driver: "mariadb", Version: "11.4.2"}
		},
		detect: func(context.Context, string, string) (database.Tool, database.Check) {
			detectCalls++
			if detectCalls == 1 {
				return database.Tool{}, database.Check{Passed: false}
			}
			return database.Tool{Name: "mariadb-dump", Version: "11.4.8"}, database.Check{Passed: true}
		},
		discover: func(context.Context, string, string) []database.DiscoveredTool { return nil },
		planInstall: func(driver string) (database.ToolInstallPlan, bool) {
			return database.ToolInstallPlan{
				Manager:  "apt-get",
				Package:  "mariadb-client",
				Summary:  "apt-get update && apt-get install -y mariadb-client",
				Commands: [][]string{{"/usr/bin/apt-get", "install", "-y", "mariadb-client"}},
			}, true
		},
		installTool: func(_ context.Context, plan database.ToolInstallPlan) error {
			installed = plan.Package == "mariadb-client"
			return nil
		},
	}

	ensureDumpTool(context.Background(), bufio.NewReader(strings.NewReader("y\n")), &output, config.DatabaseConfig{}, setup)

	if !installed || !strings.Contains(output.String(), "Installed and verified mariadb-dump 11.4.8") {
		t.Fatalf("linux install flow failed: installed=%t output=%s", installed, output.String())
	}
}
