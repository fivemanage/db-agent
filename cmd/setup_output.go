package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivemanage/db-agent/internal/database"
	"golang.org/x/term"
)

const agentBanner = ` ____    _  _____  _    ____    _    ____  _____
|  _ \  / \|_   _|/ \  | __ )  / \  / ___|| ____|
| | | |/ _ \ | | / _ \ |  _ \ / _ \ \___ \|  _|
| |_| / ___ \| |/ ___ \| |_) / ___ \ ___) | |___
|____/_/   \_\_/_/   \_\____/_/   \_\____/|_____|

    _    ____ _____ _   _ _____
   / \  / ___| ____| \ | |_   _|
  / _ \| |  _|  _| |  \| | | |
 / ___ \ |_| | |___| |\  | | |
/_/   \_\____|_____|_| \_| |_|`

type glyphSet struct {
	ok, fail, warn, info, skip, prompt string
	spinner                            []string
	full, empty                        string
}

// Redirected output, service logs, and terminals without a UTF-8 locale.
var asciiGlyphs = glyphSet{
	ok: "+", fail: "x", warn: "!", info: "-", skip: "-", prompt: ">",
	spinner: []string{"|", "/", "-", `\`}, full: "#", empty: ".",
}

// Legacy Windows console fonts (Consolas, Lucida Console) have no font
// fallback, so only WGL4 characters render reliably there.
var consoleGlyphs = glyphSet{
	ok: "√", fail: "×", warn: "!", info: "•", skip: "○", prompt: "›",
	spinner: []string{"|", "/", "-", `\`}, full: "█", empty: "░",
}

var unicodeGlyphs = glyphSet{
	ok: "✓", fail: "✗", warn: "!", info: "•", skip: "○", prompt: "›",
	spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}, full: "━", empty: "━",
}

type setupOutput struct {
	writer io.Writer
	color  bool
	// live enables in-place terminal updates such as spinners and step bars.
	live   bool
	glyphs *glyphSet
}

func newSetupOutput(writer io.Writer) setupOutput {
	output := setupOutput{writer: writer}
	if os.Getenv("TERM") == "dumb" {
		return output
	}
	file, ok := writer.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(file.Fd())) || !enableVirtualTerminal(file.Fd()) {
		return output
	}

	glyphs := terminalGlyphs()
	output.live = true
	output.glyphs = &glyphs
	output.color = os.Getenv("NO_COLOR") == ""

	return output
}

func (o setupOutput) icons() glyphSet {
	if o.glyphs == nil {
		return asciiGlyphs
	}

	return *o.glyphs
}

func (o setupOutput) styled(text, code string) string {
	if !o.color {
		return text
	}

	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (o setupOutput) banner() {
	fmt.Fprintf(o.writer, "\n%s\n\n", o.styled(agentBanner, "1;36"))
}

func (o setupOutput) title(title, description string) {
	o.banner()
	fmt.Fprintf(o.writer, "  %s\n", o.styled(title, "1"))
	if description != "" {
		fmt.Fprintf(o.writer, "  %s\n", o.styled(description, "2"))
	}
}

func (o setupOutput) step(number, total int, title string) {
	if !o.live {
		fmt.Fprintf(o.writer, "\n[%d/%d] %s\n", number, total, title)
		return
	}

	const width = 24
	filled := width * number / total
	icons := o.icons()
	bar := o.styled(strings.Repeat(icons.full, filled), "36") + o.styled(strings.Repeat(icons.empty, width-filled), "90")
	fmt.Fprintf(o.writer, "\n  %s  %s  %s\n", bar, o.styled(fmt.Sprintf("%d/%d", number, total), "90"), o.styled(title, "1"))
}

func (o setupOutput) section(title string) {
	fmt.Fprintf(o.writer, "\n  %s\n", o.styled(title, "1"))
}

func (o setupOutput) ok(message string)   { o.line(o.icons().ok, "32", message) }
func (o setupOutput) fail(message string) { o.line(o.icons().fail, "31", message) }
func (o setupOutput) warn(message string) { o.line(o.icons().warn, "1;33", message) }
func (o setupOutput) info(message string) { o.line(o.icons().info, "36", message) }

func (o setupOutput) line(icon, code, message string) {
	fmt.Fprintf(o.writer, "  %s %s\n", o.styled(icon, code), message)
}

// note prints secondary guidance aligned with status messages.
func (o setupOutput) note(message string) {
	fmt.Fprintf(o.writer, "    %s\n", o.styled(message, "2"))
}

func (o setupOutput) detail(label, value string) {
	fmt.Fprintf(o.writer, "    %s %s\n", o.styled(fmt.Sprintf("%-10s", label), "2"), value)
}

func (o setupOutput) check(check database.Check) {
	icon, code := o.icons().ok, "32"
	switch {
	case check.Skipped:
		icon, code = o.icons().skip, "90"
	case !check.Passed:
		icon, code = o.icons().fail, "31"
	}
	fmt.Fprintf(o.writer, "  %s %s: %s\n", o.styled(icon, code), check.Name, o.styled(check.Message, "2"))
}

// spin animates label until the returned stop function is called. Nothing
// else may write to the output while it runs. Non-live output stays silent so
// logs only receive the final result.
func (o setupOutput) spin(label string) (stop func()) {
	if !o.live {
		return func() {}
	}

	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		frames := o.icons().spinner
		started := time.Now()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for frame := 0; ; frame++ {
			elapsed := ""
			if seconds := int(time.Since(started).Seconds()); seconds >= 2 {
				elapsed = o.styled(fmt.Sprintf(" %ds", seconds), "90")
			}
			fmt.Fprintf(o.writer, "\r\x1b[2K  %s %s%s", o.styled(frames[frame%len(frames)], "36"), label, elapsed)
			select {
			case <-done:
				fmt.Fprint(o.writer, "\r\x1b[2K")
				return
			case <-ticker.C:
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}

func (o setupOutput) ask(label, hint string) {
	if hint != "" {
		hint = " " + o.styled("("+hint+")", "2")
	}
	fmt.Fprintf(o.writer, "  %s %s%s: ", o.styled(o.icons().prompt, "36"), label, hint)
}

func prompt(input *bufio.Reader, output io.Writer, label, fallback string) (string, error) {
	newSetupOutput(output).ask(label, displayText(fallback))

	line, err := input.ReadString('\n')
	if err != nil {
		return "", errors.New("setup input ended; no settings were changed")
	}

	line = strings.TrimSpace(line)
	if line == "" {
		line = fallback
	}

	return line, nil
}

func confirm(input *bufio.Reader, output io.Writer, question string, defaultYes bool) (bool, error) {
	hint := "y/N"
	if defaultYes {
		hint = "Y/n"
	}
	newSetupOutput(output).ask(question, hint)

	line, err := input.ReadString('\n')
	if err != nil {
		return false, errors.New("setup input ended; no settings were changed")
	}

	answer := strings.TrimSpace(line)
	if answer == "" {
		return defaultYes, nil
	}

	return affirmative(answer), nil
}

// displayText keeps user-supplied names readable while escaping anything that
// could act as a terminal control sequence.
func displayText(value string) string {
	quoted := strconv.Quote(value)
	if quoted[1:len(quoted)-1] == value {
		return value
	}

	return quoted
}
