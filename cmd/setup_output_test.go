package cmd

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/fivemanage/db-agent/internal/database"
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

			output.title("Database backup setup", "Credentials stay on this machine.")
			output.step(2, 5, "Connect your database")
			output.section("Dashboard restores")
			output.detail("Server", "127.0.0.1:3306")
			output.ok("Connected to FiveManage")
			output.warn("Save an offline recovery copy.")
			output.fail("Database connection needs attention.")
			stop := output.spin("Testing the connection")
			stop()

			text := buffer.String()
			if strings.Count(text, agentBanner) != 1 || strings.Index(text, agentBanner) > strings.Index(text, "Database backup setup") {
				t.Fatal("setup must show one ASCII banner before its heading")
			}
			for _, want := range []string{
				"Database backup setup\n", "[2/5] Connect your database", "Dashboard restores",
				"Server     127.0.0.1:3306", "  + Connected to FiveManage",
				"  ! Save an offline recovery copy.", "  x Database connection needs attention.",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in setup output: %s", want, text)
				}
			}
			if strings.ContainsAny(text, "\x1b\r") || strings.Contains(text, "Testing the connection") {
				t.Fatal("plain setup output contains terminal control sequences or spinner frames")
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

	newSetupOutput(file).ok("Enrollment saved.")

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "  + Enrollment saved.\n" {
		t.Fatalf("redirected file output: %q, error: %v", data, err)
	}
}

func TestSetupColorAddsOnlyStyling(t *testing.T) {
	var plain, colored bytes.Buffer
	for _, output := range []setupOutput{{writer: &plain}, {writer: &colored, color: true}} {
		output.title("Setup", "Your credentials stay local.")
		output.step(4, 5, "Check prerequisites")
		output.section("Results")
		output.ok("A readable result.")
		output.info("A readable result.")
		output.warn("A readable result.")
		output.fail("A readable result.")
		output.note("A readable result.")
		output.check(database.Check{Name: "Check", Skipped: true, Message: "A readable result."})
		output.ask("Question", "Y/n")
	}

	style := regexp.MustCompile(`\x1b\[[0-9;]+m`)
	if !strings.Contains(colored.String(), "\x1b[") || style.ReplaceAllString(colored.String(), "") != plain.String() {
		t.Fatal("color changed setup content or introduced non-styling terminal controls")
	}
}

func TestLiveStepShowsProgressBar(t *testing.T) {
	var buffer bytes.Buffer
	output := setupOutput{writer: &buffer, live: true, glyphs: &asciiGlyphs}

	output.step(2, 4, "Connect your database")

	if !strings.Contains(buffer.String(), "############............  2/4  Connect your database") {
		t.Fatalf("step progress: %q", buffer.String())
	}
}

func TestLiveSpinnerClearsItsLineWhenStopped(t *testing.T) {
	var buffer lockedBuffer
	output := setupOutput{writer: &buffer, live: true, glyphs: &asciiGlyphs}

	stop := output.spin("Testing the connection")
	stop()
	stop()
	output.ok("Connected")

	text := buffer.String()
	if !strings.Contains(text, "Testing the connection") || !strings.HasSuffix(text, "\r\x1b[2K  + Connected\n") {
		t.Fatalf("spinner output: %q", text)
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestConfirmUsesDefaultOnlyForEmptyAnswer(t *testing.T) {
	for _, test := range []struct {
		input      string
		defaultYes bool
		want       bool
	}{
		{"\n", true, true}, {"\n", false, false}, {"n\n", true, false}, {"yes\n", false, true}, {"maybe\n", true, false},
	} {
		var output bytes.Buffer
		got, err := confirm(bufio.NewReader(strings.NewReader(test.input)), &output, "Continue?", test.defaultYes)
		if err != nil || got != test.want {
			t.Fatalf("confirm(%q, %t) = %t, %v", test.input, test.defaultYes, got, err)
		}
	}
	if _, err := confirm(bufio.NewReader(strings.NewReader("")), &bytes.Buffer{}, "Continue?", true); err == nil {
		t.Fatal("ended input was treated as an answer")
	}
}

func TestDisplayTextEscapesTerminalControls(t *testing.T) {
	if displayText("game_db") != "game_db" || displayText("game\x1b[31m") != `"game\x1b[31m"` {
		t.Fatal("display text did not keep plain names or escape control sequences")
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
