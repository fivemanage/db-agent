//go:build !windows

package cmd

import (
	"os"
	"strings"
)

func enableVirtualTerminal(_ uintptr) bool {
	return true
}

func terminalGlyphs() glyphSet {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if value := os.Getenv(name); value != "" {
			value = strings.ToLower(value)
			if strings.Contains(value, "utf-8") || strings.Contains(value, "utf8") {
				return unicodeGlyphs
			}
			return asciiGlyphs
		}
	}

	return asciiGlyphs
}
