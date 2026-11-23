//go:build !windows

package database

import "os/exec"

func lookupTool(name string) (string, error) {
	return exec.LookPath(name)
}
