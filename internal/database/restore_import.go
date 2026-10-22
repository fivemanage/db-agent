package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/restoreplan"
	"github.com/go-sql-driver/mysql"
)

var (
	ErrRestoreExists     = errors.New("restore destination or import identity already exists; nothing was reused or overwritten; inspect the isolated destination before requesting a new job")
	ErrRestoreImport     = errors.New("isolated restore import failed; the partial destination is retained for inspection, never automatically retried or dropped")
	ErrRestoreValidation = errors.New("isolated restore validation failed; the destination was not declared successful")
	ErrRestoreUncertain  = errors.New("cannot confirm restore SQL has stopped or its import account is locked; inspect the isolated destination and local server before cutover")
	ErrRestorePolicy     = errors.New("local SQL server cannot enforce the supported restore-account locking policy; no import was started")
)

const DefaultRestoreStatementTimeout = 5 * time.Minute
const restoreCleanupTimeout = 5 * time.Second

var restoreDestinationPattern = regexp.MustCompile(`^fm_restore_[a-f0-9]{32}$`)

type RestoreWarningError struct {
	Statement int
	Code      uint16
}

func (e *RestoreWarningError) Error() string {
	return fmt.Sprintf("restore statement %d raised server warning %d; destination was not declared successful", e.Statement, e.Code)
}

func (e *RestoreWarningError) Unwrap() error { return ErrRestoreValidation }

// ImportRestore never executes recovered SQL through the provisioning authority.
// The two hooks persist write intent before account DDL and validation progress
// before result checks. Success includes closing/killing the child connection
// and locking its account; the retained locked definer must not be dropped.
func ImportRestore(ctx context.Context, backup config.DatabaseConfig, access config.RestoreConfig, plan *restoreplan.Plan, statementTimeout time.Duration,
	beforeWrite, beforeValidation func(context.Context) error,
) (err error) {
	if err := access.ValidateFor(backup); err != nil {
		return err
	}
	if plan == nil || !restoreDestinationPattern.MatchString(plan.Destination()) || beforeWrite == nil || beforeValidation == nil || statementTimeout < 0 {
		return ErrRestoreImport
	}
	if statementTimeout == 0 {
		statementTimeout = DefaultRestoreStatementTimeout
	}
	settings := config.DatabaseConfig{Driver: access.Driver, Host: access.Host, Port: access.Port, User: access.User, Password: access.Password}
	authority, err := openDatabase(settings)
	if err != nil {
		return ErrRestoreUnavailable
	}
	defer authority.Close()
	probe, stop := context.WithTimeout(ctx, Timeout)
	err = checkRestoreAccess(probe, authority, settings)
	stop()
	if err != nil {
		return err
	}
	conn, err := authority.Conn(ctx)
	if err != nil {
		return ErrRestoreUnavailable
	}
	defer conn.Close()
	var identity, version string
	var exists int
	probe, stop = context.WithTimeout(ctx, Timeout)
	err = conn.QueryRowContext(probe, "SELECT CURRENT_USER(), VERSION()").Scan(&identity, &version)
	if err == nil {
		err = conn.QueryRowContext(probe, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", plan.Destination()).Scan(&exists)
	}
	stop()
	at := strings.LastIndexByte(identity, '@')
	if err != nil || at < 1 || identity[:at] != settings.User || validateClientHost(identity[at+1:]) != nil {
		return ErrRestorePrivileges
	}
	if exists != 0 {
		return ErrRestoreExists
	}
	if !supportsRestoreLock(settings.Driver, version) {
		return ErrRestorePolicy
	}
	digest := sha256.Sum256([]byte(plan.Destination()))
	user := "fmr_" + hex.EncodeToString(digest[:14])
	account := "'" + user + "'@'" + identity[at+1:] + "'"
	var material [32]byte
	if _, err := rand.Read(material[:]); err != nil {
		return ErrRestoreImport
	}
	child := settings
	child.User, child.Password = user, hex.EncodeToString(material[:])
	clear(material[:])
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := beforeWrite(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	probe, stop = context.WithTimeout(ctx, Timeout)
	_, err = conn.ExecContext(probe, "CREATE USER "+account+" IDENTIFIED BY '"+child.Password+"'")
	stop()
	if err != nil {
		// A lost CREATE response can leave an account, but doesn't prove this
		// call created it. Never alter/drop a possibly pre-existing identity.
		var server *mysql.MySQLError
		if errors.As(err, &server) && server.Number == 1396 {
			return ErrRestoreExists
		}
		if !errors.As(err, &server) {
			return ErrRestoreUncertain
		}
		return ErrRestoreImport
	}
	defer func() {
		// Use a new, bounded context/connection: work may be canceled and its
		// authority connection may have been invalidated by the driver.
		cleanup, cancel := context.WithTimeout(context.Background(), restoreCleanupTimeout)
		defer cancel()
		_, lockErr := conn.ExecContext(cleanup, "ALTER USER "+account+" ACCOUNT LOCK")
		if lockErr != nil {
			locker, openErr := openDatabase(settings)
			if openErr == nil {
				defer locker.Close()
				_, lockErr = locker.ExecContext(cleanup, "ALTER USER "+account+" ACCOUNT LOCK")
			}
		}
		if lockErr != nil {
			err = &restoreImportError{ErrRestoreUncertain, err}
		}
	}()
	probe, stop = context.WithTimeout(ctx, Timeout)
	_, err = conn.ExecContext(probe, "GRANT "+restorePrivileges+" ON "+quoteGrantIdentifier(plan.Destination())+".* TO "+account)
	stop()
	if err != nil {
		return ErrRestorePrivileges
	}
	return importAsChild(ctx, child, plan, statementTimeout, beforeValidation)
}

func supportsRestoreLock(driver, version string) bool {
	actual, numeric := serverVersion(version)
	major, minor := versionNumbers(numeric)
	parts := strings.Split(numeric, ".")
	patch := 0
	if len(parts) > 2 {
		patch, _ = strconv.Atoi(parts[2])
	}
	if actual != driver {
		return false
	}
	if driver == "mysql" {
		return major > 5 || major == 5 && minor == 7 && patch >= 6
	}
	return major > 10 || major == 10 && (minor > 4 || minor == 4 && patch >= 2)
}

func importAsChild(ctx context.Context, settings config.DatabaseConfig, plan *restoreplan.Plan, statementTimeout time.Duration, beforeValidation func(context.Context) error) (err error) {
	db, err := openDatabaseReadTimeout(settings, 0)
	if err != nil {
		return ErrRestoreImport
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return ErrRestoreImport
	}
	var thread uint64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&thread); err != nil || thread == 0 {
		conn.Close()
		return ErrRestoreImport
	}
	defer func() {
		conn.Close()
		db.Close()
		if stopErr := stopRestoreThread(settings, thread); stopErr != nil {
			err = &restoreImportError{ErrRestoreUncertain, err}
		}
	}()
	var identity, version string
	if err := conn.QueryRowContext(ctx, "SELECT CURRENT_USER(), VERSION()").Scan(&identity, &version); err != nil || !strings.HasPrefix(identity, settings.User+"@") {
		return ErrRestorePrivileges
	}
	_, numeric := serverVersion(version)
	major, _ := versionNumbers(numeric)
	if settings.Driver == "mariadb" || major >= 8 {
		var role sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT CURRENT_ROLE()").Scan(&role); err != nil || role.Valid && role.String != "" && role.String != "NONE" {
			return ErrRestorePrivileges
		}
	}
	if err := checkRestoreChildGrants(ctx, conn, plan.Destination()); err != nil {
		return err
	}
	// Row-count validation on a session with uncommitted inserts would be a
	// false success: closing that connection can roll those rows back.
	if _, err := conn.ExecContext(ctx, "SET SESSION autocommit = 1"); err != nil {
		return ErrRestoreImport
	}
	ordinal := 0
	if err := plan.ForEach(ctx, func(statement string) error {
		ordinal++
		query, cancel := context.WithTimeout(ctx, statementTimeout)
		defer cancel()
		if _, err := conn.ExecContext(query, statement); err != nil {
			return err
		}
		code, err := restoreWarning(query, conn)
		if err != nil {
			return ErrRestoreValidation
		}
		if code != 0 {
			// MySQL 8 recommends adding zero-date modes to strict mode (3135).
			// Keep legacy literal zero-date fidelity: ignore only this advisory
			// on our exact setup statement, never warnings from recovered DML.
			if ordinal == 1 && statement == restoreplan.SessionModeSQL && code == 3135 {
				return nil
			}
			return &RestoreWarningError{Statement: ordinal, Code: code}
		}
		return nil
	}); err != nil {
		return &restoreImportError{ErrRestoreImport, err}
	}
	if err := beforeValidation(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRestore(ctx, conn, identity, plan, statementTimeout); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &restoreImportError{ErrRestoreValidation, err}
	}
	return nil
}

// Closing a client socket isn't proof that the server stopped a canceled DDL or
// insert. A second connection of the same child user can kill only its own
// thread and confirm it disappeared, without PROCESS/CONNECTION_ADMIN grants.
func stopRestoreThread(settings config.DatabaseConfig, thread uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), restoreCleanupTimeout)
	defer cancel()
	db, err := openDatabase(settings)
	if err != nil {
		return ErrRestoreUncertain
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return ErrRestoreUncertain
	}
	defer conn.Close()
	_, killErr := conn.ExecContext(ctx, "KILL CONNECTION "+strconv.FormatUint(thread, 10))
	if killErr != nil {
		var server *mysql.MySQLError
		if !errors.As(killErr, &server) || server.Number != 1094 {
			return ErrRestoreUncertain
		}
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var active int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID = ?", thread).Scan(&active); err != nil {
			return ErrRestoreUncertain
		}
		if active == 0 {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ErrRestoreUncertain
		}
	}
}

type restoreImportError struct {
	message error
	cause   error
}

func (e *restoreImportError) Error() string        { return e.message.Error() }
func (e *restoreImportError) Unwrap() error        { return e.cause }
func (e *restoreImportError) Is(target error) bool { return target == e.message }
