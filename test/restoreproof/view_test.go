//go:build integration

package restoreproof

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/restoreplan"
)

func sourceViewHash(f *fixture, names []string) string {
	var queries []string
	for _, name := range names {
		queries = append(queries, "SHOW CREATE VIEW "+qi(f.source)+"."+qi(name),
			"SELECT * FROM "+qi(f.source)+"."+qi(name)+" ORDER BY id")
	}
	return f.hashQueries(queries)
}

func TestNativeViewsPreflightRebuildsUsableConfinedObjects(t *testing.T) {
	f := newFixture(t)
	options := planOptions(f)
	f.tables(f.admin, f.source)
	f.exec(f.admin, "SET SESSION collation_connection = utf8mb4_bin")
	source := qi(f.source)
	for _, statement := range []string{
		"CREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW " + source + ".z_balances AS SELECT " + source + ".players.id AS id, " + source + ".players.credits AS credits, " + source + ".players.note AS note, 'original.players; # literal' AS marker FROM " + source + ".players WHERE " + source + ".players.id>=1",
		"CREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW " + source + ".a_summary AS SELECT b.id AS id, b.credits+1 AS adjusted, b.note AS note, b.marker AS marker FROM " + source + ".z_balances AS b WHERE b.credits>0",
		"CREATE SQL SECURITY DEFINER VIEW " + source + ".joined AS SELECT p.id AS id, i.id AS item_id FROM " + source + ".players AS p LEFT JOIN " + source + ".items AS i ON p.id=i.owner",
		"CREATE SQL SECURITY DEFINER VIEW " + source + ".case_sensitive AS SELECT p.id AS id, ('a'='A') AS same FROM " + source + ".players AS p",
	} {
		f.exec(f.admin, statement)
	}
	names := []string{"a_summary", "case_sensitive", "joined", "z_balances"}
	before := f.snapshotSchema(false) + sourceViewHash(f, names)
	verified := f.verifiedDump()

	plan, err := restoreplan.Prepare(f.ctx, bytes.NewReader(verified), options)
	if err != nil {
		t.Fatalf("native view preflight failed: %v", err)
	}
	if len(plan.Tables()) != 3 || len(plan.Views()) != len(names) {
		t.Fatal("native view placeholder leaked into the actual table/view inventory")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", f.target) != 0 {
		t.Fatal("view preflight created a destination")
	}
	writer := f.writer()
	if err := plan.ForEach(f.ctx, func(statement string) error { _, err := writer.ExecContext(f.ctx, statement); return err }); err != nil {
		t.Fatalf("native view import failed under destination-only privileges (server error %d)", errorNumber(err))
	}
	t.Cleanup(func() {
		if _, err := f.admin.Exec("DROP DATABASE " + qi(f.target)); err != nil {
			t.Error("cannot remove test-owned native view destination")
		}
	})
	if f.scalar(writer, "SELECT adjusted FROM "+qi(f.target)+".a_summary WHERE id=1") != 101 || f.scalar(writer, "SELECT item_id FROM "+qi(f.target)+".joined WHERE id=1") != 1 {
		t.Fatal("restored view dependencies, qualified references, expressions, or joins are unusable")
	}
	if f.scalar(writer, "SELECT same FROM "+qi(f.target)+".case_sensitive WHERE id=1") != 0 {
		t.Fatal("restored view did not preserve source collation-sensitive literal semantics")
	}
	var marker, identity string
	if err := writer.QueryRowContext(f.ctx, "SELECT marker FROM "+qi(f.target)+".a_summary WHERE id=1").Scan(&marker); err != nil || marker != "original.players; # literal" {
		t.Fatal("schema-like literal data in a native view was changed")
	}
	if err := writer.QueryRowContext(f.ctx, "SELECT CURRENT_USER()").Scan(&identity); err != nil {
		t.Fatal("cannot inspect restricted view definer identity")
	}
	for _, view := range plan.Views() {
		var definer, security, collation string
		if err := f.admin.QueryRowContext(f.ctx, "SELECT DEFINER, SECURITY_TYPE, COLLATION_CONNECTION FROM information_schema.VIEWS WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", f.target, view.Name).Scan(&definer, &security, &collation); err != nil || definer != identity || security != "DEFINER" || collation != "utf8mb4_bin" {
			t.Fatal("native view retained a privileged definer, invoker context, or wrong creation collation")
		}
	}
	f.exec(writer, "UPDATE "+qi(f.target)+".players SET credits=200 WHERE id=1")
	if f.scalar(writer, "SELECT adjusted FROM "+qi(f.target)+".a_summary WHERE id=1") != 201 {
		t.Fatal("restored view still reads the original database instead of destination data")
	}
	if f.snapshotSchema(false)+sourceViewHash(f, names) != before {
		t.Fatal("native view dump/preflight/import/use changed source or outside data/definitions")
	}
	t.Log("native view dump -> authenticated recovery -> preflight -> restricted import passed: placeholders removed, views usable, restricted definers/collation preserved, source unchanged")
}

func TestUnsafeNativeViewDefinitionsFailBeforeDestinationCreation(t *testing.T) {
	for _, kind := range []string{"cross_schema", "invoker", "security_function"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			options := planOptions(f)
			f.tables(f.admin, f.source)
			statement := "CREATE VIEW " + qi(f.source) + ".unsafe AS SELECT id FROM " + qi(f.other) + ".sentinel"
			switch kind {
			case "invoker":
				statement = "CREATE SQL SECURITY INVOKER VIEW " + qi(f.source) + ".unsafe AS SELECT id FROM " + qi(f.source) + ".players"
			case "security_function":
				statement = "CREATE VIEW " + qi(f.source) + ".unsafe AS SELECT id, CURRENT_USER() AS identity_value FROM " + qi(f.source) + ".players"
			}
			f.exec(f.admin, statement)
			before := f.snapshotSchema(false) + sourceViewHash(f, []string{"unsafe"})
			verified := f.verifiedDump()

			plan, err := restoreplan.Prepare(f.ctx, bytes.NewReader(verified), options)
			if plan != nil || !errors.Is(err, restoreplan.ErrUnsupported) || !strings.Contains(err.Error(), "CreateView") {
				t.Fatal("unsafe native view returned an executable or partial plan")
			}
			if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", f.target) != 0 || f.snapshotSchema(false)+sourceViewHash(f, []string{"unsafe"}) != before {
				t.Fatal("unsafe native view preflight created a destination or changed the source")
			}
		})
	}
}
