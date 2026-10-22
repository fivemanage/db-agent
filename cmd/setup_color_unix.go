//go:build !windows

package cmd

func enableSetupColor(_ uintptr) bool {
	return true
}
