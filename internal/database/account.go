package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"

	"github.com/fivemanage/db-agent/internal/config"
)

const backupAccount = "'db_agent_backup'@'localhost'"

// BackupAccountSQL previews operations without generating or revealing a
// password. Execution substitutes a CSPRNG-generated hex password.
func BackupAccountSQL(driver, version string, databases []string) ([]string, error) {
	if len(databases) == 0 {
		return nil, errors.New("confirm at least one application database before creating an account")
	}
	statements := []string{"CREATE USER " + backupAccount + " IDENTIFIED BY '<generated password>'"}
	seen := map[string]bool{}
	for _, name := range databases {
		if name == "" || systemDatabase(name) || strings.ContainsFunc(name, unicode.IsControl) {
			return nil, errors.New("scoped account requires application database names without control characters")
		}
		if !seen[name] {
			statements = append(statements, "GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON "+quoteGrantIdentifier(name)+".* TO "+backupAccount)
			seen[name] = true
		}
	}
	major, minor := versionNumbers(version)
	parts := strings.Split(version, ".")
	patch := 0
	if len(parts) > 2 {
		patch, _ = strconv.Atoi(parts[2])
	}
	var routine string
	switch {
	case driver == "mysql" && (major > 8 || major == 8 && (minor > 0 || patch >= 20)):
		routine = "SHOW_ROUTINE ON *.*"
	case driver == "mysql" && major == 5 && minor == 7:
		routine = "SELECT ON `mysql`.`proc`"
	case driver == "mariadb" && (major > 11 || major == 11 && minor >= 3):
		routine = "SHOW CREATE ROUTINE ON *.*"
	case driver == "mariadb" && (major > 10 || major == 10 && minor >= 3):
		routine = "SELECT ON `mysql`.`proc`"
	default:
		return nil, errors.New("automatic least-privilege routine grants are unavailable for this server version; use an existing backup account and ask a local administrator for routine visibility")
	}
	return append(statements, "GRANT "+routine+" TO "+backupAccount), nil
}

// LocalBackupAccountSQL keeps the destination local while using the client host
// seen by the server, which can be a bridge address for a containerized database.
func LocalBackupAccountSQL(driver, version string, databases []string, host, clientHost string) ([]string, error) {
	ip, err := localTCPHost(host)
	if err != nil {
		return nil, err
	}
	if err := validateClientHost(clientHost); err != nil {
		return nil, err
	}
	statements, err := BackupAccountSQL(driver, version, databases)
	if err != nil {
		return nil, err
	}
	var all []string
	clientIP := net.ParseIP(clientHost)
	if strings.EqualFold(clientHost, "localhost") || clientIP != nil && clientIP.IsLoopback() {
		// Preserve both native local identities for name-resolution differences.
		all = append(all, statements...)
		if strings.EqualFold(clientHost, "localhost") {
			clientHost = ip
		}
	}
	account := "'db_agent_backup'@'" + clientHost + "'"
	for _, statement := range statements {
		if strings.HasPrefix(statement, "CREATE USER ") {
			all = append(all, "CREATE USER "+account+" IDENTIFIED BY '<generated password>'")
		} else {
			all = append(all, strings.TrimSuffix(statement, backupAccount)+account)
		}
	}
	return all, nil
}

func readClientHost(ctx context.Context, db accountExecutor) (string, error) {
	var identity string
	// CURRENT_USER() is the matched account (possibly @%), not the client host.
	if err := db.QueryRowContext(ctx, "SELECT USER()").Scan(&identity); err != nil {
		return "", errors.New("could not determine the database's client host; use an existing backup account or retry the connection check")
	}
	host := ""
	if at := strings.LastIndexByte(identity, '@'); at >= 0 {
		host = identity[at+1:]
	}
	if err := validateClientHost(host); err != nil {
		return "", err
	}
	return host, nil
}

func validateClientHost(host string) error {
	invalid := errors.New("the database did not report a safe exact client host; use an existing backup account or ask a local administrator to inspect the connection")
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return invalid
		}
		return nil
	}
	if host == "" || len(host) > 253 {
		return invalid
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return invalid
			}
		}
	}
	return nil
}

func localTCPHost(host string) (string, error) {
	if strings.EqualFold(host, "localhost") {
		return "127.0.0.1", nil
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.Equal(net.ParseIP("127.0.0.1")) || ip.Equal(net.ParseIP("::1"))) {
		return ip.String(), nil
	}
	return "", errors.New("setup connects only to a database on this machine; use localhost, 127.0.0.1, or ::1")
}

// Database-level GRANT patterns interpret _ and % as wildcards even in quoted
// identifiers. Escaping them prevents access to similarly named schemas.
func quoteGrantIdentifier(name string) string {
	name = strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(name)
	return quoteIdentifier(name)
}

type accountExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ProvisionBackupAccount never alters an existing account. The callback must
// verify and persist the generated backup credentials, not the admin settings.
// The server must still report the client host used in the approved SQL preview.
// On failure, only identities successfully created by this call are removed.
func ProvisionBackupAccount(ctx context.Context, admin config.DatabaseConfig, driver, version, clientHost string, persist func(config.DatabaseConfig) error) error {
	if err := ValidateLocalConnection(admin); err != nil {
		return err
	}
	statements, err := LocalBackupAccountSQL(driver, version, admin.Databases, admin.Host, clientHost)
	if err != nil {
		return err
	}
	db, err := openDatabase(admin)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	return provisionAccount(ctx, db, admin, driver, clientHost, statements, persist)
}

func provisionAccount(ctx context.Context, db accountExecutor, admin config.DatabaseConfig, driver, clientHost string, statements []string, persist func(config.DatabaseConfig) error) error {
	observedHost, err := readClientHost(ctx, db)
	if err != nil {
		return err
	}
	if observedHost != clientHost {
		return errors.New("the database's client host changed after SQL approval; no accounts were created; retry setup to approve the current host")
	}

	var password [32]byte
	if _, err := rand.Read(password[:]); err != nil {
		return fmt.Errorf("generate backup password: %w", err)
	}
	candidate := admin
	candidate.Driver, candidate.User, candidate.Password = driver, "db_agent_backup", hex.EncodeToString(password[:])
	candidate.Host, _ = localTCPHost(admin.Host)
	var created []string
	var failure error
	for _, statement := range statements {
		account, isCreate := strings.CutPrefix(statement, "CREATE USER ")
		if isCreate {
			account, _, _ = strings.Cut(account, " IDENTIFIED BY ")
			statement = "CREATE USER " + account + " IDENTIFIED BY '" + candidate.Password + "'"
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			if isCreate {
				// A lost response may mean CREATE succeeded. Never drop this
				// identity unless this call positively knows it created it.
				failure = errors.New("could not create the approved db_agent_backup identities; check CREATE USER privileges and whether an account already exists. Existing accounts are never changed; use an existing backup account or ask a local administrator to inspect it")
			} else {
				failure = errors.New("could not grant backup access; ask a local administrator for grant authority on the selected schemas and routine visibility, or use an existing backup account")
			}
			break
		}
		if isCreate {
			created = append(created, account)
		}
	}
	if failure == nil {
		failure = persist(candidate)
	}
	if failure == nil {
		return nil
	}
	if len(created) == 0 {
		return failure
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), Timeout)
	defer cancel()
	var incomplete bool
	for i := len(created) - 1; i >= 0; i-- {
		if _, err := db.ExecContext(cleanup, "DROP USER "+created[i]); err != nil {
			incomplete = true
		}
	}
	if incomplete {
		return fmt.Errorf("%w; cleanup failed: ask a local administrator to inspect the newly created db_agent_backup identities before retrying", failure)
	}
	return fmt.Errorf("%w; the backup identities confirmed created by this attempt were removed; retry setup or use an existing account", failure)
}
