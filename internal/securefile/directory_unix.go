//go:build !windows

package securefile

func protectDirectory(string) error { return nil }
