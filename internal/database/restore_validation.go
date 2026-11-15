package database

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/fivemanage/db-agent/internal/restoreplan"
)

// Reading a system variable with SELECT can replace the diagnostics area on
// some servers. SHOW WARNINGS is diagnostic and preserves the prior statement.
func restoreWarning(ctx context.Context, conn *sql.Conn) (uint16, error) {
	rows, err := conn.QueryContext(ctx, "SHOW WARNINGS LIMIT 1")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err()
	}
	var level, message string
	var code uint16
	if err := rows.Scan(&level, &code, &message); err != nil {
		return 0, err
	}
	return code, nil
}

func checkRestoreChildGrants(ctx context.Context, conn *sql.Conn, destination string) error {
	rows, err := conn.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER")
	if err != nil {
		return ErrRestorePrivileges
	}
	defer rows.Close()
	required := make(map[string]bool)
	for _, privilege := range strings.Split(restorePrivileges, ", ") {
		required[privilege] = false
	}
	for rows.Next() {
		var grant string
		if rows.Scan(&grant) != nil || !strings.HasPrefix(grant, "GRANT ") || strings.HasSuffix(grant, " WITH GRANT OPTION") {
			return ErrRestorePrivileges
		}
		start, end := strings.Index(grant, " ON "), strings.LastIndex(grant, " TO ")
		if start < 6 || end <= start {
			return ErrRestorePrivileges
		}
		scope := grant[start+4 : end]
		for _, privilege := range strings.Split(grant[6:start], ",") {
			privilege = strings.TrimSpace(privilege)
			if scope == "*.*" && privilege == "USAGE" {
				continue
			}
			if _, known := required[privilege]; !known || scope != quoteGrantIdentifier(destination)+".*" {
				return ErrRestorePrivileges
			}
			required[privilege] = true
		}
	}
	if rows.Err() != nil {
		return ErrRestorePrivileges
	}
	for _, present := range required {
		if !present {
			return ErrRestorePrivileges
		}
	}
	return nil
}

func validateRestore(ctx context.Context, conn *sql.Conn, identity string, plan *restoreplan.Plan, timeout time.Duration) error {
	query, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	expected := make(map[string]string)
	for _, table := range plan.Tables() {
		expected[table.Name] = "BASE TABLE"
	}
	for _, view := range plan.Views() {
		expected[view.Name] = "VIEW"
	}
	rows, err := conn.QueryContext(query, "SELECT TABLE_NAME, TABLE_TYPE, COALESCE(ENGINE, '') FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", plan.Destination())
	if err != nil {
		return ErrRestoreValidation
	}
	count := 0
	for rows.Next() {
		var name, kind, engine string
		if rows.Scan(&name, &kind, &engine) != nil || expected[name] != kind || kind == "BASE TABLE" && !strings.EqualFold(engine, "InnoDB") {
			rows.Close()
			return ErrRestoreValidation
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != len(expected) {
		return ErrRestoreValidation
	}
	for _, table := range plan.Tables() {
		var count int64
		statement, stop := context.WithTimeout(ctx, timeout)
		err := conn.QueryRowContext(statement, "SELECT COUNT(*) FROM "+quoteIdentifier(plan.Destination())+"."+quoteIdentifier(table.Name)).Scan(&count)
		stop()
		if err != nil || count != table.Rows {
			return ErrRestoreValidation
		}
	}
	for _, view := range plan.Views() {
		statement, stop := context.WithTimeout(ctx, timeout)
		var definer, security, collation string
		err := conn.QueryRowContext(statement, "SELECT DEFINER, SECURITY_TYPE, COLLATION_CONNECTION FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", plan.Destination(), view.Name).Scan(&definer, &security, &collation)
		if err != nil || definer != identity || security != "DEFINER" || collation != view.Collation {
			stop()
			return ErrRestoreValidation
		}
		rows, err := conn.QueryContext(statement, "SELECT * FROM "+quoteIdentifier(plan.Destination())+"."+quoteIdentifier(view.Name)+" LIMIT 1")
		if err != nil {
			stop()
			return ErrRestoreValidation
		}
		columns, err := rows.Columns()
		valid := err == nil && len(columns) == len(view.Columns)
		if valid {
			for i, column := range columns {
				valid = valid && column == view.Columns[i]
			}
		}
		for rows.Next() {
			values := make([]any, len(columns))
			for i := range values {
				values[i] = new(any)
			}
			valid = valid && rows.Scan(values...) == nil
		}
		valid = valid && rows.Err() == nil
		rows.Close()
		warnings, err := restoreWarning(statement, conn)
		stop()
		if !valid || err != nil || warnings != 0 {
			return ErrRestoreValidation
		}
	}
	statement, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	if err := validateRestoreTriggers(statement, conn, identity, plan); err != nil {
		return err
	}
	return nil
}

func validateRestoreTriggers(ctx context.Context, conn *sql.Conn, identity string, plan *restoreplan.Plan) error {
	expected := make(map[string]restoreplan.Trigger)
	for _, trigger := range plan.Triggers() {
		expected[trigger.Name] = trigger
	}
	rows, err := conn.QueryContext(ctx, "SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, ACTION_TIMING, EVENT_MANIPULATION, ACTION_ORDER, DEFINER, COLLATION_CONNECTION, SQL_MODE FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?", plan.Destination())
	if err != nil {
		return ErrRestoreValidation
	}
	defer rows.Close()
	for rows.Next() {
		var name, table, timing, event, definer, collation, mode string
		var order int
		if rows.Scan(&name, &table, &timing, &event, &order, &definer, &collation, &mode) != nil {
			return ErrRestoreValidation
		}
		trigger, ok := expected[name]
		if !ok || table != trigger.Table || timing != trigger.Timing || event != trigger.Event || order != trigger.Order ||
			definer != identity || collation != trigger.Collation || !sameSQLModes(mode, trigger.SQLMode) {
			return ErrRestoreValidation
		}
		delete(expected, name)
	}
	if rows.Err() != nil || len(expected) != 0 {
		return ErrRestoreValidation
	}
	return nil
}

func sameSQLModes(left, right string) bool {
	modes := make(map[string]bool)
	for _, mode := range strings.Split(left, ",") {
		modes[mode] = true
	}
	for _, mode := range strings.Split(right, ",") {
		if !modes[mode] {
			return false
		}
		delete(modes, mode)
	}
	return len(modes) == 0
}
