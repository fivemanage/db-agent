package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/go-sql-driver/mysql"
)

var (
	ErrRestoreUnavailable = errors.New("local database is unavailable for restore-access verification; check the database service and local connection")
	ErrRestorePrivileges  = errors.New("restore provisioning access is unavailable or unsafe; use a separate direct-grant account with only the displayed restore permissions, without roles or production access")
)

const restoreGrantScope = "`fm\\_restore\\_%`.*"
const restorePrivileges = "CREATE, INSERT, SELECT, UPDATE, DELETE, REFERENCES, CREATE VIEW, SHOW VIEW"

// RestoreAccessSQL is a setup template, not a migration or an import executor.
// The dedicated account may provision destination-only principals; backup SQL
// must never run through its broader account-management connection.
func RestoreAccessSQL() []string {
	account := "'db_agent_restore'@'<exact local client host>'"
	return []string{
		"CREATE USER " + account + " IDENTIFIED BY '<private restore password>'",
		"GRANT CREATE USER ON *.* TO " + account,
		"GRANT " + restorePrivileges + " ON " + restoreGrantScope + " TO " + account + " WITH GRANT OPTION",
	}
}

// CheckRestoreAccess is read-only: no users, grants, databases, or keys change.
// Re-run before provisioning; a saved authorization is not proof of live grants.
func CheckRestoreAccess(ctx context.Context, backup config.DatabaseConfig, access config.RestoreConfig) error {
	if err := access.ValidateFor(backup); err != nil {
		return err
	}
	settings := config.DatabaseConfig{Driver: access.Driver, Host: access.Host, Port: access.Port, User: access.User, Password: access.Password}
	db, err := openDatabase(settings)
	if err != nil {
		return ErrRestoreUnavailable
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	return checkRestoreAccess(ctx, db, settings)
}

func checkRestoreAccess(ctx context.Context, db *sql.DB, settings config.DatabaseConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := db.PingContext(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var server *mysql.MySQLError
		if errors.As(err, &server) && (server.Number == 1045 || server.Number == 1698) {
			return config.ErrRestoreAuthorization
		}
		return ErrRestoreUnavailable
	}
	var identity, version string
	if err := db.QueryRowContext(ctx, "SELECT CURRENT_USER(), VERSION()").Scan(&identity, &version); err != nil {
		return ErrRestorePrivileges
	}
	at := strings.LastIndexByte(identity, '@')
	if at < 1 || identity[:at] != settings.User || validateClientHost(identity[at+1:]) != nil {
		return ErrRestorePrivileges
	}
	driver, numeric := serverVersion(version)
	major, minor := versionNumbers(numeric)
	supported := driver == "mysql" && (major > 5 || major == 5 && minor >= 7) || driver == "mariadb" && (major > 10 || major == 10 && minor >= 3)
	if driver != settings.Driver || !supported {
		return ErrRestorePrivileges
	}
	if driver == "mariadb" || major >= 8 {
		var role sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT CURRENT_ROLE()").Scan(&role); err != nil || role.Valid && role.String != "NONE" && role.String != "" {
			return ErrRestorePrivileges
		}
	}
	if driver == "mysql" && major >= 8 {
		var partialRevokes bool
		if err := db.QueryRowContext(ctx, "SELECT @@global.partial_revokes").Scan(&partialRevokes); err != nil || partialRevokes {
			return ErrRestorePrivileges
		}
	}
	grants, err := readGrants(ctx, db)
	if err != nil || !restoreAuthorityGrants(grants) {
		return ErrRestorePrivileges
	}
	return nil
}

func restoreAuthorityGrants(grants []string) bool {
	createUser := false
	required := make(map[string]bool)
	for _, privilege := range strings.Split(restorePrivileges, ", ") {
		required[privilege] = false
	}
	for _, grant := range grants {
		start, end := strings.Index(grant, " ON "), strings.LastIndex(grant, " TO ")
		if !strings.HasPrefix(grant, "GRANT ") || start < 6 || end <= start {
			return false
		}
		scope := grant[start+4 : end]
		delegate := strings.HasSuffix(grant, " WITH GRANT OPTION")
		for _, privilege := range strings.Split(grant[6:start], ",") {
			privilege = strings.TrimSpace(privilege)
			switch scope {
			case "*.*":
				if delegate || privilege != "USAGE" && privilege != "CREATE USER" {
					return false
				}
				createUser = createUser || privilege == "CREATE USER"
			case restoreGrantScope:
				if _, known := required[privilege]; !known || !delegate {
					return false
				}
				required[privilege] = true
			default:
				return false
			}
		}
	}
	if !createUser {
		return false
	}
	for _, present := range required {
		if !present {
			return false
		}
	}
	return true
}
