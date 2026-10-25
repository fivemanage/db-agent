package cmd

import (
	"os"

	"golang.org/x/sys/windows"
)

func enableVirtualTerminal(fd uintptr) bool {
	handle := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}

	return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}

// Go writes console output as UTF-16, but the legacy console host (the
// Windows Server default) cannot fall back to fonts with dingbats or braille.
func terminalGlyphs() glyphSet {
	if os.Getenv("WT_SESSION") != "" || os.Getenv("TERM_PROGRAM") != "" {
		return unicodeGlyphs
	}

	return consoleGlyphs
}
