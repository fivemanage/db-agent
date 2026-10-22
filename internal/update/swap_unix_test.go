//go:build !windows

package update

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelperRestoresOldExecutableWhenRestartFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "db-agent")
	staged := filepath.Join(dir, "db-agent.new")
	old := []byte("old executable")
	newBinary := []byte("new executable")
	if err := os.WriteFile(target, old, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, newBinary, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(newBinary)
	err := RunHelper(target, staged, hex.EncodeToString(hash[:]), "1.1.0", Restart{ConfigPath: filepath.Join(filepath.Dir(target), "config.yaml")})
	if err == nil || !strings.Contains(err.Error(), "old binary restored") {
		t.Fatalf("RunHelper() = %v, want rollback error", err)
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != string(old) {
		t.Fatalf("installed executable after rollback = %q, %v", contents, err)
	}
}

func TestRunHelperInstallsVerifiedExecutableWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "db-agent")
	staged := filepath.Join(dir, "db-agent.new")
	old := []byte("old executable")
	newBinary := []byte("#!/bin/sh\nprintf '1.1.0\\n'\n")
	if err := os.WriteFile(target, old, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, newBinary, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(newBinary)
	if err := RunHelper(target, staged, hex.EncodeToString(hash[:]), "1.1.0", Restart{NoRestart: true, ConfigPath: filepath.Join(filepath.Dir(target), "config.yaml")}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != string(newBinary) {
		t.Fatalf("installed executable = %q, %v", contents, err)
	}
	backup, err := os.ReadFile(target + ".rollback")
	if err != nil || string(backup) != string(old) {
		t.Fatalf("rollback executable = %q, %v", backup, err)
	}
}
