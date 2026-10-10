package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/go-sql-driver/mysql"
)

const Timeout = 10 * time.Second

type Check struct {
	Name    string
	Passed  bool
	Skipped bool
	Message string
}

type Report struct {
	Checks         []Check
	Driver         string
	Version        string
	ClientHost     string
	Databases      []string
	CanCreateUser  bool
	Administrative bool
}

func (r Report) Passed() bool {
	for _, check := range r.Checks {
		if !check.Passed {
			return false
		}
	}

	return len(r.Checks) > 0
}

// Inspect accepts both candidate wizard settings and persisted local settings.
func Inspect(ctx context.Context, settings config.DatabaseConfig) Report {
	if err := settings.Validate(); err != nil {
		return unavailable("Database connectivity", err.Error())
	}

	db, err := openDatabase(settings)
	if err != nil {
		return unavailable("Database connectivity", err.Error())
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	return inspect(ctx, db, settings)
}

func openDatabase(settings config.DatabaseConfig) (*sql.DB, error) {
	c := mysql.NewConfig()
	c.User, c.Passwd = settings.User, settings.Password
	host := settings.Host
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	c.Net, c.Addr = "tcp", net.JoinHostPort(host, strconv.Itoa(settings.Port))
	c.Timeout, c.ReadTimeout, c.WriteTimeout = Timeout, Timeout, Timeout
	// Driver logging can include untrusted server messages. Diagnostics expose
	// only classified errors, never raw errors or the DSN.
	c.Logger = log.New(io.Discard, "", 0)
	connector, err := mysql.NewConnector(c)
	if err != nil {
		return nil, errors.New("invalid local connection settings; check the host, port, and account")
	}

	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)

	return db, nil
}

func unavailable(name, message string) Report {
	r := Report{Checks: []Check{{Name: name, Message: message}}}
	for _, n := range []string{"Database authentication", "Database version", "Database selection", "Required grants"} {
		r.Checks = append(r.Checks, Check{Name: n, Skipped: true, Message: "Complete the database connection check first."})
	}

	return r
}

func inspect(ctx context.Context, db *sql.DB, settings config.DatabaseConfig) Report {
	if err := db.PingContext(ctx); err != nil {
		r := unavailable("Database connectivity", Explain(err))
		var serverErr *mysql.MySQLError
		if errors.As(err, &serverErr) {
			r.Checks[0] = Check{Name: "Database connectivity", Passed: true, Message: "Server reached."}
			r.Checks[1] = Check{Name: "Database authentication", Message: Explain(err)}
		}

		return r
	}

	r := Report{Checks: []Check{
		{Name: "Database connectivity", Passed: true, Message: "Server reached."},
		{Name: "Database authentication", Passed: true, Message: "Local account authenticated."},
	}}
	// Host discovery gates account creation, not use of an existing account.
	r.ClientHost, _ = readClientHost(ctx, db)

	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		r.Checks = append(r.Checks, Check{Name: "Database version", Message: Explain(err)})
	} else {
		r.Driver, r.Version = serverVersion(version)
		major, minor := versionNumbers(r.Version)
		supported := (r.Driver == "mysql" && (major > 5 || major == 5 && minor >= 7)) || (r.Driver == "mariadb" && (major > 10 || major == 10 && minor >= 3))
		message := fmt.Sprintf("%s %s", r.Driver, r.Version)
		if !supported {
			message = "Unsupported server version; use MySQL 5.7+ or MariaDB 10.3+."
		}
		r.Checks = append(r.Checks, Check{Name: "Database version", Passed: supported, Message: message})
	}

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		r.Checks = append(r.Checks, Check{Name: "Database selection", Message: Explain(err)}, Check{Name: "Required grants", Skipped: true, Message: "Resolve database discovery first."})
		return r
	}

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			r.Checks = append(r.Checks, Check{Name: "Database selection", Message: "Cannot read visible databases; check server compatibility."}, Check{Name: "Required grants", Skipped: true, Message: "Resolve database discovery first."})
			return r
		}

		if !systemDatabase(name) {
			r.Databases = append(r.Databases, name)
		}
	}

	err = rows.Err()
	rows.Close()
	if err != nil {
		r.Checks = append(r.Checks, Check{Name: "Database selection", Message: Explain(err)}, Check{Name: "Required grants", Skipped: true, Message: "Resolve database discovery first."})
		return r
	}

	selection := Check{Name: "Database selection", Passed: true, Message: fmt.Sprintf("%d explicitly selected database(s).", len(settings.Databases))}
	if len(settings.Databases) == 0 {
		selection.Passed = false
		selection.Message = "No database selected; run db-agent configure and confirm the backup scope."
	}

	for _, name := range settings.Databases {
		// USE distinguishes an absent schema from a schema hidden by grants.
		if systemDatabase(name) || name == "" {
			selection.Passed, selection.Message = false, "Select application databases only; system databases cannot be backed up by this path."
			break
		}

		if _, err := db.ExecContext(ctx, "USE "+quoteIdentifier(name)); err != nil {
			selection.Passed, selection.Message = false, Explain(err)
			break
		}
	}

	r.Checks = append(r.Checks, selection)
	if !selection.Passed || r.Driver == "" {
		r.Checks = append(r.Checks, Check{Name: "Required grants", Skipped: true, Message: "Resolve database selection and version first."})
		return r
	}

	grants, err := readGrants(ctx, db)
	if err != nil {
		r.Checks = append(r.Checks, Check{Name: "Required grants", Message: Explain(err)})
	} else {
		r.CanCreateUser = hasGrant(grants, "CREATE USER", "*", "*")
		r.Administrative = administrativeGrants(grants)
		missing := missingGrants(grants, settings.Databases, r.Driver)
		check := Check{Name: "Required grants", Passed: len(missing) == 0, Message: "Logical dump privileges verified (single-transaction, routines, triggers, events, no tablespaces)."}
		if len(missing) > 0 {
			check.Message = "Ask a local DB administrator to grant " + strings.Join(missing, ", ") + ". Role-only grants are not verified; grant backup privileges directly to this account."
		}
		r.Checks = append(r.Checks, check)
	}

	return r
}

func Explain(err error) string {
	var dbErr *mysql.MySQLError
	if errors.As(err, &dbErr) {
		switch dbErr.Number {
		case 1045, 1698:
			return "Authentication rejected; check the local username/password and allowed account host. Use a password-authenticated backup account."
		case 1049:
			return "Selected database does not exist; check its name or run db-agent configure again."
		case 1044, 1142, 1143, 1227, 1370:
			return "Insufficient database privileges; ask a local administrator to verify the selected database exists and grant the required backup access. Servers may hide missing databases behind access denied."
		case 1040:
			return "Server connection limit reached; free connections or increase the server limit, then retry."
		default:
			return "Database operation rejected; check server compatibility and the local account grants."
		}
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "Database hostname could not be resolved; check database.host (normally 127.0.0.1)."
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return "Connection refused; check the host/port and that MySQL or MariaDB is running and listening on TCP."
	}

	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return "Database connection timed out; check the host/port, firewall, and server availability."
	}

	if errors.Is(err, context.Canceled) {
		return "Database check canceled; retry when ready."
	}

	return "Database connection failed; check the host/port, server availability, and account settings."
}

func serverVersion(value string) (string, string) {
	driver := "mysql"
	if strings.Contains(strings.ToLower(value), "mariadb") {
		driver = "mariadb"
		value = strings.TrimPrefix(value, "5.5.5-")
	}

	// Only expose the numeric version, not arbitrary server-provided text.
	n := 0
	for n < len(value) && (value[n] >= '0' && value[n] <= '9' || value[n] == '.') {
		n++
	}

	return driver, value[:n]
}

func versionNumbers(version string) (int, int) {
	parts := strings.Split(version, ".")
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}

	return major, minor
}

func systemDatabase(name string) bool {
	switch strings.ToLower(name) {
	case "information_schema", "performance_schema", "mysql", "sys":
		return true
	}

	return false
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
