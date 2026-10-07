//go:build !windows

package config

import (
	"os"

	"github.com/fivemanage/db-agent/internal/securefile"
)

func createPrivateFile(path string) (*os.File, error) {
	return securefile.OpenNew(path)
}

func replacePrivateFile(from, to string) error {
	return securefile.Replace(from, to)
}
