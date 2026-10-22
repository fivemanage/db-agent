package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/securefile"
)

func restoreSettings() (DatabaseConfig, RestoreConfig) {
	backup := DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "backup-secret", Databases: []string{"game"}}
	access := RestoreConfig{Authorized: true, Driver: backup.Driver, Host: backup.Host, Port: backup.Port, User: "restore_setup", Password: "restore-secret"}
	return backup, access
}

func TestRestoreAuthorizationIsSeparateLocalAndServerBound(t *testing.T) {
	backup, access := restoreSettings()
	if err := access.ValidateFor(backup); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RestoreConfig){
		func(r *RestoreConfig) { r.Authorized = false },
		func(r *RestoreConfig) { r.User = "backup" },
		func(r *RestoreConfig) { r.User = "ROOT" },
		func(r *RestoreConfig) { r.User = "invalid'username" },
		func(r *RestoreConfig) { r.Password = "" },
		func(r *RestoreConfig) { r.Password = "private\x00password" },
		func(r *RestoreConfig) { r.Host = "remote.example.com" },
		func(r *RestoreConfig) { r.Host = "127.0.0.2" },
		func(r *RestoreConfig) { r.Port++ },
		func(r *RestoreConfig) { r.Driver = "mariadb" },
	} {
		changed := access
		mutate(&changed)
		if err := changed.ValidateFor(backup); !errors.Is(err, ErrRestoreAuthorization) || strings.Contains(err.Error(), "restore-secret") {
			t.Fatal("unconfirmed, shared, remote, or rebound restore authorization was accepted")
		}
	}
	for _, scope := range [][]string{nil, {"mysql"}, {"FM_RESTORE_reserved"}} {
		changed := backup
		changed.Databases = scope
		if err := access.ValidateFor(changed); !errors.Is(err, ErrRestoreAuthorization) {
			t.Fatal("invalid production/restore scope was accepted")
		}
	}
	access.Host = "localhost"
	if err := access.ValidateFor(backup); err != nil {
		t.Fatal("equivalent loopback host was rejected")
	}
	access.Host, backup.Host = "::1", "::1"
	if err := access.ValidateFor(backup); err != nil {
		t.Fatal("local IPv6 binding was rejected")
	}
}

func TestSaveRestoreAccessPreservesBackupKeyAndOtherSettings(t *testing.T) {
	backup, access := restoreSettings()
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "# keep comment\nagent_credential: dac_unchanged\nfuture_setting: keep\nrestore:\n  future_restore_setting: keep\nencryption:\n  key_file: original.key\n  key_id: existing-id\n  key_acknowledged: true\nbackup:\n  schedule: '1 2 * * *'\n"
	if err := securefile.Create(path, []byte(original)); err != nil {
		t.Fatal(err)
	}
	if err := SaveDatabase(path, backup); err != nil {
		t.Fatal(err)
	}
	before, err := LoadDiagnostic(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRestoreAccess(path, backup, access); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDiagnostic(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Database, before.Database) || loaded.Encryption != before.Encryption || loaded.AgentCredential != before.AgentCredential || loaded.Backup != before.Backup || loaded.Restore != access {
		t.Fatal("restore setup changed backup/enrollment/key settings or lost authorization")
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "future_setting: keep") || !strings.Contains(string(data), "future_restore_setting: keep") || !strings.Contains(string(data), "# keep comment") {
		t.Fatal("restore save lost unrelated config or comments")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("restore credentials were not saved privately")
		}
	}
	if err := SaveRestoreAccess(path, backup, access); err != nil {
		t.Fatal("reconfirming unchanged authorization failed")
	}
	backup.Port++
	if err := SaveRestoreAccess(path, backup, access); err == nil {
		t.Fatal("changed endpoint was authorized without matching the saved database")
	}
	if data, err := json.Marshal(access); err != nil || string(data) != "{}" {
		t.Fatal("local restore settings entered a JSON payload")
	}
	if data, err := json.Marshal(loaded); err != nil || strings.Contains(string(data), "restore-secret") || strings.Contains(string(data), "restore_setup") {
		t.Fatal("config serialized restore credentials for a remote payload")
	}
	if text := fmt.Sprintf("%+v %#v", access, access); strings.Contains(text, "restore-secret") || strings.Contains(text, "restore_setup") {
		t.Fatal("formatting restore authorization leaked credentials")
	}
}

func TestRestoreSaveRejectsChangedDatabaseSnapshot(t *testing.T) {
	backup, access := restoreSettings()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := securefile.Create(path, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	changed := backup
	changed.User = "different_backup"
	if err := SaveDatabase(path, changed); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRestoreAccess(path, backup, access); err == nil {
		t.Fatal("stale wizard snapshot saved authorization")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("failed restore setup modified configuration")
	}
}
