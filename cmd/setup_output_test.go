package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestSetupOutputRemainsReadableWithoutColor(t *testing.T) {
	for _, env := range []struct{ name, noColor, term string }{
		{"redirected", "", "xterm-256color"},
		{"no color", "1", "xterm-256color"},
		{"dumb terminal", "", "dumb"},
	} {
		t.Run(env.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", env.noColor)
			t.Setenv("TERM", env.term)
			var buffer bytes.Buffer
			output := newSetupOutput(&buffer)

			output.title("Database backup setup", "Enrollment -> Database -> Recovery key -> Checks -> Service")
			output.step(2, 5, "Connect your database", "Credentials stay on this machine.")
			output.section("Review connection")
			output.detail("Password", "(hidden)")
			output.status("OK", "Enrollment saved.")
			output.status("WARN", "Save an offline recovery copy.")
			output.status("FAIL", "Database connection needs attention.")

			text := buffer.String()
			if strings.Count(text, agentBanner) != 1 || strings.Index(text, agentBanner) > strings.Index(text, "Database backup setup") {
				t.Fatal("setup must show one ASCII banner before its heading")
			}
			for _, want := range []string{
				"Database backup setup\n=====================",
				"[2/5] Connect your database", "Review connection",
				"Password     (hidden)", "[OK] Enrollment saved.",
				"[WARN] Save an offline recovery copy.", "[FAIL] Database connection needs attention.",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in setup output: %s", want, text)
				}
			}
			if strings.ContainsAny(text, "\x1b\r") {
				t.Fatal("plain setup output contains terminal control sequences")
			}
		})
	}
}

func TestSetupOutputDoesNotColorRedirectedFiles(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	path := filepath.Join(t.TempDir(), "setup.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })

	newSetupOutput(file).status("OK", "Enrollment saved.")

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "  [OK] Enrollment saved.\n" {
		t.Fatalf("redirected file output: %q, error: %v", data, err)
	}
}

func TestSetupColorAddsOnlyStyling(t *testing.T) {
	var plain, colored bytes.Buffer
	for _, output := range []setupOutput{{writer: &plain}, {writer: &colored, color: true}} {
		output.title("Setup", "Your credentials stay local.")
		output.step(4, 5, "Check prerequisites", "Checking before service installation...")
		output.section("Results")
		for _, status := range []string{"OK", "INFO", "WARN", "PASS", "FAIL", "SKIP"} {
			output.status(status, "A readable result.")
		}
	}

	style := regexp.MustCompile(`\x1b\[[0-9;]+m`)
	if !strings.Contains(colored.String(), "\x1b[") || style.ReplaceAllString(colored.String(), "") != plain.String() {
		t.Fatal("color changed setup content or introduced non-styling terminal controls")
	}
}

func TestAgentBannerFitsBasicTerminals(t *testing.T) {
	for _, line := range strings.Split(agentBanner, "\n") {
		if len(line) > 64 {
			t.Fatalf("banner exceeds a compact terminal width: %d columns", len(line))
		}
		for _, character := range line {
			if character < ' ' || character > '~' {
				t.Fatal("banner must contain only printable ASCII and newlines")
			}
		}
	}
}

func TestHelpShowsBannerAndPreservesCommandHelp(t *testing.T) {
	for _, command := range []*cobra.Command{rootCmd, installCmd, configureCmd} {
		t.Run(command.Name(), func(t *testing.T) {
			previous := command.OutOrStdout()
			t.Cleanup(func() { command.SetOut(previous) })
			var output bytes.Buffer
			command.SetOut(&output)

			if err := command.Help(); err != nil {
				t.Fatal(err)
			}

			text := output.String()
			if strings.Count(text, agentBanner) != 1 || !strings.Contains(text, "Usage:") || !strings.Contains(text, "--config") {
				t.Fatalf("help is missing its banner, usage, or flags: %s", text)
			}
			if strings.Index(text, agentBanner) > strings.Index(text, "Usage:") || strings.ContainsAny(text, "\x1b\r") {
				t.Fatal("redirected help must start with a plain ASCII banner")
			}
		})
	}
}
