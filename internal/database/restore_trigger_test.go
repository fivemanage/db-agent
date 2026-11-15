package database

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fivemanage/db-agent/internal/restoreplan"
)

func triggerValidationPlan(t *testing.T) *restoreplan.Plan {
	t.Helper()
	input := `CREATE DATABASE original CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; USE original;
CREATE TABLE players (id INT) ENGINE=InnoDB;
CREATE TABLE audit_log (id INT) ENGINE=InnoDB;
SET collation_connection=utf8mb4_bin;
SET sql_mode='STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION';
CREATE TRIGGER record_insert AFTER INSERT ON players FOR EACH ROW INSERT INTO audit_log VALUES (NEW.id);`
	plan, err := restoreplan.Prepare(context.Background(), strings.NewReader(input), restoreplan.Options{
		Driver: "mysql", Source: "original", Destination: "fm_restore_" + strings.Repeat("a", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestRestoreTriggerValidationChecksInventoryIdentityAndCreationContext(t *testing.T) {
	plan := triggerValidationPlan(t)
	for _, mode := range []string{"", "missing", "extra", "duplicate", "table", "timing", "event", "order", "definer", "collation", "sql_mode", "query_error", "row_error"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			name, table, timing, event, order := "record_insert", "players", "AFTER", "INSERT", 1
			definer, collation, sqlMode := "child@localhost", "utf8mb4_bin", "NO_ENGINE_SUBSTITUTION,STRICT_ALL_TABLES"
			switch mode {
			case "table":
				table = "outside"
			case "timing":
				timing = "BEFORE"
			case "event":
				event = "UPDATE"
			case "order":
				order = 2
			case "definer":
				definer = "root@localhost"
			case "collation":
				collation = "utf8mb4_general_ci"
			case "sql_mode":
				sqlMode = ""
			}
			rows := sqlmock.NewRows([]string{"name", "table", "timing", "event", "order", "definer", "collation", "mode"})
			if mode != "missing" {
				rows.AddRow(name, table, timing, event, order, definer, collation, sqlMode)
			}
			if mode == "extra" || mode == "duplicate" {
				if mode == "extra" {
					name = "unknown"
				}
				rows.AddRow(name, table, timing, event, order, definer, collation, sqlMode)
			}
			if mode == "row_error" {
				rows.RowError(0, errors.New("private server response"))
			}
			query := mock.ExpectQuery("SELECT TRIGGER_NAME,.*FROM information_schema.TRIGGERS").WithArgs(plan.Destination())
			if mode == "query_error" {
				query.WillReturnError(errors.New("private server response"))
			} else {
				query.WillReturnRows(rows).RowsWillBeClosed()
			}
			err = validateRestoreTriggers(context.Background(), conn, "child@localhost", plan)
			if (err == nil) != (mode == "") || err != nil && !errors.Is(err, ErrRestoreValidation) {
				t.Fatalf("invalid trigger inventory/security context became success: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestoreTriggerBinaryLogPolicyNeverAddsGlobalPrivileges(t *testing.T) {
	for _, mode := range []string{"no_binlog", "trusted", "untrusted", "query_error"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			query := mock.ExpectQuery(regexp.QuoteMeta("SELECT @@global.log_bin, @@global.log_bin_trust_function_creators"))
			if mode == "query_error" {
				query.WillReturnError(errors.New("private server response"))
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"binary_log", "trusted"}).AddRow(mode != "no_binlog", mode == "trusted"))
			}
			err = checkRestoreTriggerPolicy(context.Background(), conn)
			allowed := mode == "no_binlog" || mode == "trusted"
			if (err == nil) != allowed || !allowed && !errors.Is(err, ErrRestoreTriggerPolicy) {
				t.Fatalf("unexpected trigger binary log policy: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
