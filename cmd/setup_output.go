package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

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

type setupOutput struct {
	writer io.Writer
	color  bool
}

func newSetupOutput(writer io.Writer) setupOutput {
	output := setupOutput{writer: writer}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return output
	}
	if file, ok := writer.(interface{ Fd() uintptr }); ok && term.IsTerminal(int(file.Fd())) {
		output.color = enableSetupColor(file.Fd())
	}

	return output
}

func (o setupOutput) styled(text, code string) string {
	if !o.color {
		return text
	}

	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (o setupOutput) title(title, description string) {
	o.banner()
	fmt.Fprintf(o.writer, "%s\n%s\n%s\n", o.styled(title, "1;36"), strings.Repeat("=", len(title)), description)
}

func (o setupOutput) banner() {
	fmt.Fprintf(o.writer, "\n%s\n\n", o.styled(agentBanner, "1;36"))
}

func (o setupOutput) step(number, total int, title, description string) {
	fmt.Fprintf(o.writer, "\n%s %s\n  %s\n", o.styled(fmt.Sprintf("[%d/%d]", number, total), "1;36"), o.styled(title, "1"), description)
}

func (o setupOutput) section(title string) {
	fmt.Fprintf(o.writer, "\n%s\n", o.styled(title, "1"))
}

func (o setupOutput) status(status, message string) {
	fmt.Fprintf(o.writer, "  %s %s\n", o.badge(status), message)
}

func (o setupOutput) badge(status string) string {
	code := "36"
	switch status {
	case "OK", "PASS":
		code = "32"
	case "FAIL":
		code = "31"
	case "WARN", "SKIP":
		code = "33"
	}

	return o.styled("["+status+"]", code)
}

func (o setupOutput) detail(label, value string) {
	fmt.Fprintf(o.writer, "    %-12s %s\n", label, value)
}
