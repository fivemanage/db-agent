package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/fivemanage/db-agent/internal/config"
)

// LocalRestoreAccountSQL previews only agent-authored provisioning SQL. Restore
// data access stays separate from both the backup account and the import user.
func LocalRestoreAccountSQL(backup config.DatabaseConfig, clientHost string) ([]string, error) {
	if err := ValidateLocalConnection(backup); err != nil {
		return nil, err
	}
	if err := validateClientHost(clientHost); err != nil {
		return nil, err
	}
	access := config.RestoreConfig{Authorized: true, Driver: backup.Driver, Host: backup.Host, Port: backup.Port, User: "db_agent_restore", Password: "<generated password>"}
	if err := access.ValidateFor(backup); err != nil {
		return nil, err
	}
	return restoreAccountSQL(clientHost, "<generated password>"), nil
}

// ProvisionRestoreAccount never alters existing identities or backup grants.
// The callback must verify live restore access before saving it privately.
func ProvisionRestoreAccount(ctx context.Context, admin, backup config.DatabaseConfig, clientHost string, persist func(config.RestoreConfig) error) error {
	if err := ValidateLocalConnection(admin); err != nil {
		return err
	}
	adminHost, _ := localTCPHost(admin.Host)
	backupHost, _ := localTCPHost(backup.Host)
	if adminHost != backupHost || admin.Port != backup.Port || admin.Driver != backup.Driver {
		return errors.New("restore setup must use the confirmed local database server; no accounts were created")
	}
	if _, err := LocalRestoreAccountSQL(backup, clientHost); err != nil {
		return err
	}
	db, err := openDatabase(admin)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	return provisionRestoreAccount(ctx, db, backup, clientHost, persist)
}

func provisionRestoreAccount(ctx context.Context, db accountExecutor, backup config.DatabaseConfig, clientHost string, persist func(config.RestoreConfig) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	observed, err := readClientHost(ctx, db)
	if err != nil {
		return err
	}
	if observed != clientHost {
		return errors.New("the database's client host changed after SQL approval; no restore account was created; retry installation to approve the current host")
	}
	var password [32]byte
	if _, err := rand.Read(password[:]); err != nil {
		return fmt.Errorf("generate restore password: %w", err)
	}
	access := config.RestoreConfig{Authorized: true, Driver: backup.Driver, Host: backup.Host, Port: backup.Port, User: "db_agent_restore", Password: hex.EncodeToString(password[:])}
	statements := restoreAccountSQL(clientHost, access.Password)
	if _, err := db.ExecContext(ctx, statements[0]); err != nil {
		// An existing identity or a lost CREATE response is not ours to remove.
		return errors.New("could not create db_agent_restore; check CREATE USER privileges and whether the exact-host account already exists. Existing accounts are never changed; inspect locally or use configure --restore-access to authorize an existing dedicated account")
	}
	var failure error
	for _, statement := range statements[1:] {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			failure = errors.New("could not grant restore access; a local administrator needs authority to delegate CREATE USER and the displayed restore-namespace permissions")
			break
		}
	}
	if failure == nil {
		failure = ctx.Err()
	}
	if failure == nil {
		failure = persist(access)
	}
	if failure == nil {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), Timeout)
	defer cancel()
	if _, err := db.ExecContext(cleanup, "DROP USER 'db_agent_restore'@'"+clientHost+"'"); err != nil {
		return fmt.Errorf("%w; cleanup failed: ask a local administrator to inspect the newly created db_agent_restore account before retrying", failure)
	}
	return fmt.Errorf("%w; the restore account confirmed created by this attempt was removed; backup settings are preserved", failure)
}
