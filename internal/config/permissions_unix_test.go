//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigReadRefusesBroadPermissionsAndSymlinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const secret = "dac_private-config-secret"
	if err := SaveEnrollment(path, "https://api.example.com", secret); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(path); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("public config accepted or secret disclosed")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "linked.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(link); err == nil {
		t.Fatal("linked private config accepted")
	}
}
