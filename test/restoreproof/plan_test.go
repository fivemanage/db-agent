//go:build integration

package restoreproof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/fivemanage/db-agent/internal/restoreplan"
)

func planOptions(f *fixture) restoreplan.Options {
	digest := sha256.Sum256([]byte(f.target))
	f.target = "fm_restore_" + hex.EncodeToString(digest[:16])
	return restoreplan.Options{Driver: f.engine, Source: f.source, Destination: f.target}
}

func TestNativeDumpPreflightImportsOnlyIntoFreshDestination(t *testing.T) {
	f := newFixture(t)
	options := planOptions(f)
	f.tables(f.admin, f.source)
	// Exercise common game-schema types/defaults against the actual native dump
	// syntax, in addition to the adversarial string/binary/FK fixtures.
	f.exec(f.admin, "CREATE TABLE "+qi(f.source)+".profiles (id INT AUTO_INCREMENT PRIMARY KEY, role ENUM('user','admin') NOT NULL DEFAULT 'user', balance DECIMAL(12,2) NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, note LONGTEXT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	f.exec(f.admin, "SET SESSION sql_mode = CONCAT(@@sql_mode, ',NO_AUTO_VALUE_ON_ZERO')")
	f.exec(f.admin, "INSERT INTO "+qi(f.source)+".profiles VALUES (0, 'admin', -12.50, '2026-10-22 12:00:00', 'schema-like original.players')")
	before := f.snapshotSchema(false)
	verified := f.verifiedDump()

	plan, err := restoreplan.Prepare(f.ctx, bytes.NewReader(verified), options)
	if err != nil {
		t.Fatalf("native dump preflight failed: %v", err)
	}
	if got := f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", f.target); got != 0 {
		t.Fatal("preflight created a destination before complete validation")
	}
	writer := f.writer()
	f.exec(writer, "SET SESSION foreign_key_checks = 0, unique_checks = 0")
	err = plan.ForEach(f.ctx, func(statement string) error {
		_, err := writer.ExecContext(f.ctx, statement)
		return err
	})
	if err != nil {
		t.Fatalf("validated native dump could not be imported with destination-only privileges (server error %d)", errorNumber(err))
	}
	t.Cleanup(func() {
		if _, err := f.admin.Exec("DROP DATABASE " + qi(f.target)); err != nil {
			t.Error("cannot remove test-owned import destination")
		}
	})
	if got := plan.Tables(); len(got) != 4 {
		t.Fatal("preflight lost part of the native table inventory")
	}
	for _, table := range plan.Tables() {
		count := f.scalar(writer, "SELECT COUNT(*) FROM "+qi(f.target)+"."+qi(table.Name))
		if int64(count) != table.Rows {
			t.Fatal("destination row count differs from the validated plan")
		}
	}
	if f.scalar(writer, "SELECT COUNT(*) FROM "+qi(f.target)+".profiles WHERE id=0 AND role='admin' AND balance=-12.50 AND created_at='2026-10-22 12:00:00' AND note='schema-like original.players'") != 1 {
		t.Fatal("native numeric/enum/timestamp/auto-increment-zero values changed during import")
	}
	var sourceNote, targetNote, binary, reference string
	if err := f.admin.QueryRowContext(f.ctx, "SELECT note FROM "+qi(f.source)+".players WHERE id=1").Scan(&sourceNote); err != nil {
		t.Fatal("cannot inspect source data")
	}
	if err := writer.QueryRowContext(f.ctx, "SELECT note, HEX(payload) FROM "+qi(f.target)+".players WHERE id=1").Scan(&targetNote, &binary); err != nil || targetNote != sourceNote || binary != "0001FEFF" {
		t.Fatal("preflight changed a schema-like string or binary value")
	}
	if err := f.admin.QueryRowContext(f.ctx, "SELECT REFERENCED_TABLE_SCHEMA FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA = ? AND TABLE_NAME='items' AND REFERENCED_TABLE_NAME IS NOT NULL", f.target).Scan(&reference); err != nil || reference != f.target {
		t.Fatal("native foreign-key reference escaped the destination")
	}
	if _, err := writer.ExecContext(f.ctx, "INSERT INTO "+qi(f.target)+".items VALUES (2,999)"); errorNumber(err) != 1452 {
		t.Fatal("planned import silently disabled foreign-key enforcement")
	}
	f.exec(writer, "UPDATE "+qi(f.target)+".players SET credits=777 WHERE id=1")
	if err := plan.ForEach(f.ctx, func(statement string) error { _, err := writer.ExecContext(f.ctx, statement); return err }); !errors.Is(err, restoreplan.ErrOutput) || errorNumber(err) != 1007 {
		t.Fatal("replayed import did not fail at fresh database creation")
	}
	if f.scalar(writer, "SELECT credits FROM "+qi(f.target)+".players WHERE id=1") != 777 {
		t.Fatal("replayed plan overwrote an existing destination")
	}
	if f.snapshotSchema(false) != before {
		t.Fatal("native dump preflight/import changed source or outside data/definitions")
	}
	t.Log("actual authenticated native dump preflight/import passed: fresh destination, FK checks on, exact data preserved, source unchanged, replay blocked")
}

func TestUnsupportedNativeObjectDumpFailsBeforeAnyImport(t *testing.T) {
	f := newFixture(t)
	options := planOptions(f)
	f.tables(f.admin, f.source)
	f.objects(f.admin, f.source)
	before := f.snapshot()
	verified := f.verifiedDump()

	plan, err := restoreplan.Prepare(f.ctx, bytes.NewReader(verified), options)
	if plan != nil || !errors.Is(err, restoreplan.ErrUnsupported) {
		t.Fatal("unsupported native objects produced a partially executable plan")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", f.target) != 0 || f.snapshot() != before {
		t.Fatal("unsupported native dump changed source data or created a partial destination")
	}
}
