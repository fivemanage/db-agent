package database

import (
	"context"
	"database/sql"
	"strings"
)

func readGrants(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []string
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// The diagnostic contract is a logical, nonlocking dump with --no-tablespaces.
// Physical backup grants and filesystem access require separate validation.
func missingGrants(grants, databases []string, driver string) []string {
	var missing []string
	for _, name := range databases {
		for _, privilege := range []string{"SELECT", "SHOW VIEW", "TRIGGER", "EVENT"} {
			if !hasGrant(grants, privilege, name, "*") {
				// Do not echo arbitrary schema names in remediation (or GRANT text,
				// which on some server versions includes authentication hashes).
				missing = appendUnique(missing, privilege+" on the selected database(s)")
			}
		}
	}
	routines := hasGrant(grants, "SELECT", "*", "*") || hasGrant(grants, "SELECT", "mysql", "proc")
	if driver == "mysql" {
		routines = routines || hasGrant(grants, "SHOW_ROUTINE", "*", "*")
	} else {
		routines = routines || hasGrant(grants, "SHOW CREATE ROUTINE", "*", "*")
	}
	if !routines {
		if driver == "mysql" {
			missing = append(missing, "routine visibility (SHOW_ROUTINE on MySQL 8.0.20+, or SELECT on mysql.proc on MySQL 5.7)")
		} else {
			missing = append(missing, "routine visibility (SELECT on mysql.proc, or SHOW CREATE ROUTINE on MariaDB 11.3+)")
		}
	}
	return missing
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func administrativeGrants(grants []string) bool {
	for _, grant := range grants {
		if strings.HasSuffix(grant, " WITH GRANT OPTION") {
			return true
		}
		if !strings.HasPrefix(grant, "GRANT ") {
			continue
		}
		start := strings.Index(grant, " ON ")
		if start < 0 {
			// A role may carry administrative privileges that SHOW GRANTS
			// does not expand. Do not persist credentials on that assumption.
			return true
		}
		if !strings.HasPrefix(grant[start:], " ON *.* TO ") {
			continue
		}
		for _, privilege := range strings.Split(grant[6:start], ",") {
			switch strings.TrimSpace(privilege) {
			case "USAGE", "SELECT", "SHOW DATABASES", "SHOW_ROUTINE", "SHOW CREATE ROUTINE":
			default:
				return true
			}
		}
	}
	return false
}

func hasGrant(grants []string, privilege, schema, table string) bool {
	for _, grant := range grants {
		if !strings.HasPrefix(grant, "GRANT ") {
			continue
		}
		start := strings.Index(grant, " ON ")
		end := strings.LastIndex(grant, " TO ")
		if start < 0 || end < start {
			continue
		}
		scope := grant[start+4 : end]
		if scope != "*.*" && scope != quoteIdentifier(schema)+".*" && scope != quoteGrantIdentifier(schema)+".*" && scope != quoteIdentifier(schema)+"."+quoteIdentifier(table) && scope != quoteIdentifier(schema)+"."+table {
			continue
		}
		for _, p := range strings.Split(grant[6:start], ",") {
			p = strings.TrimSpace(p)
			if p == privilege || p == "ALL PRIVILEGES" {
				return true
			}
		}
	}
	return false
}
