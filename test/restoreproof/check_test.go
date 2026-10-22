//go:build integration

package restoreproof

import (
	"fmt"
	"testing"

	"github.com/fivemanage/db-agent/internal/restorejob"
)

func TestExecutorRestoresNativeJSONChecksAndFulltextIndex(t *testing.T) {
	f := newFixture(t)
	f.tables(f.admin, f.source)
	f.exec(f.admin, "CREATE TABLE "+qi(f.source)+".json_documents (id INT PRIMARY KEY, payload LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin, CONSTRAINT payload CHECK (json_valid(payload))) ENGINE=InnoDB")
	f.exec(f.admin, "CREATE TABLE "+qi(f.source)+".native_documents (id INT PRIMARY KEY, payload JSON) ENGINE=InnoDB")
	f.exec(f.admin, "CREATE TABLE "+qi(f.source)+".news_articles (id INT PRIMARY KEY, title VARCHAR(255), summary TEXT, content LONGTEXT, image_urls LONGTEXT CHECK (json_valid(image_urls)), FULLTEXT KEY idx_search (title,summary,content)) ENGINE=InnoDB")
	const document = `{"text":"original.players; quote ' and unicode 🙂"}`
	for _, table := range []string{"json_documents", "native_documents"} {
		f.exec(f.admin, "INSERT INTO "+qi(f.source)+"."+qi(table)+" VALUES (1,?),(2,NULL)", document)
	}
	f.exec(f.admin, "INSERT INTO "+qi(f.source)+".news_articles VALUES (1,'galacticquasar','summary','article',?)", document)
	snapshot := func() string {
		return f.snapshotSchema(false) + f.hashQueries([]string{
			"SELECT id, payload FROM " + qi(f.source) + ".json_documents ORDER BY id",
			"SHOW CREATE TABLE " + qi(f.source) + ".json_documents",
			"SELECT id, payload FROM " + qi(f.source) + ".native_documents ORDER BY id",
			"SHOW CREATE TABLE " + qi(f.source) + ".native_documents",
			"SELECT id, title, summary, content, image_urls FROM " + qi(f.source) + ".news_articles ORDER BY id",
			"SHOW CREATE TABLE " + qi(f.source) + ".news_articles",
		})
	}
	x := newExecutionFixture(f, f.nativeDump())
	registerExecutionCleanup(x)
	before, grantsBefore := snapshot(), backupGrantHash(f, x.settings.Database)

	state, err := x.runner(restorejob.Options{}).Run(f.ctx, x.job)
	if err != nil || state.Phase != restorejob.Succeeded {
		t.Fatalf("native JSON-check restore failed: phase=%s problem=%s error=%v", state.Phase, state.Problem, err)
	}
	target := qi(x.job.Destination())
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".json_documents WHERE id=1 AND payload=?", document) != 1 ||
		f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".json_documents WHERE id=2 AND payload IS NULL") != 1 ||
		f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".native_documents WHERE id=1 AND JSON_VALID(payload)") != 1 {
		t.Fatal("native JSON check or JSON alias restore changed data or NULL semantics")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=? AND TABLE_NAME='json_documents' AND CONSTRAINT_NAME='payload' AND CONSTRAINT_TYPE='CHECK'", x.job.Destination()) != 1 {
		t.Fatal("named JSON check was omitted from the destination")
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME='news_articles' AND INDEX_NAME='idx_search' AND INDEX_TYPE='FULLTEXT'", x.job.Destination()) != 3 ||
		f.scalar(f.admin, "SELECT COUNT(*) FROM "+target+".news_articles WHERE MATCH(title,summary,content) AGAINST('+galacticquasar' IN BOOLEAN MODE)") != 1 {
		t.Fatal("restored FULLTEXT index was omitted or unusable")
	}
	want := uint16(3819)
	if f.engine == "mariadb" {
		want = 4025
	}
	for _, statement := range []string{
		"INSERT INTO " + target + ".json_documents VALUES (3,'invalid-json')",
		"UPDATE " + target + ".json_documents SET payload='invalid-json' WHERE id=1",
	} {
		if _, err := f.admin.ExecContext(f.ctx, statement); errorNumber(err) != want {
			t.Fatalf("restored JSON check did not enforce writes (server error %d)", errorNumber(err))
		}
	}
	assertExecutionAccountLocked(t, x)
	if snapshot() != before || backupGrantHash(f, x.settings.Database) != grantsBefore {
		t.Fatal("JSON-check restore changed source/outside data, definitions, or backup access")
	}
}

func TestExecutorJSONChecksOverrideDisabledMariaDBEnforcement(t *testing.T) {
	f := newFixture(t)
	if f.engine != "mariadb" {
		t.Skip("check_constraint_checks is MariaDB-specific")
	}
	f.tables(f.admin, f.source)
	f.exec(f.admin, "CREATE TABLE "+qi(f.source)+".json_documents (id INT PRIMARY KEY, payload LONGTEXT, CHECK (json_valid(payload))) ENGINE=InnoDB")
	f.exec(f.admin, "SET SESSION check_constraint_checks = 0")
	f.exec(f.admin, "INSERT INTO "+qi(f.source)+".json_documents VALUES (1,'invalid-json')")
	f.exec(f.admin, "SET SESSION check_constraint_checks = 1")
	x := newExecutionFixture(f, f.nativeDump())
	registerExecutionCleanup(x)
	before := f.hashQueries([]string{"SELECT id, payload FROM " + qi(f.source) + ".json_documents", "SHOW CREATE TABLE " + qi(f.source) + ".json_documents"})
	global := f.scalar(f.admin, "SELECT @@GLOBAL.check_constraint_checks")
	f.exec(f.admin, "SET GLOBAL check_constraint_checks = 0")
	t.Cleanup(func() {
		if _, err := f.admin.Exec(fmt.Sprintf("SET GLOBAL check_constraint_checks = %d", global)); err != nil {
			t.Error("cannot reset disposable fixture CHECK enforcement")
		}
	})

	state, err := x.runner(restorejob.Options{}).Run(f.ctx, x.job)
	if err == nil || state.Phase != restorejob.Failed || state.Problem != restorejob.ImportFailed || !state.WritesStarted {
		t.Fatalf("invalid JSON bypassed CHECK enforcement: phase=%s problem=%s error=%v", state.Phase, state.Problem, err)
	}
	if f.scalar(f.admin, "SELECT COUNT(*) FROM "+qi(x.job.Destination())+".json_documents") != 0 ||
		f.hashQueries([]string{"SELECT id, payload FROM " + qi(f.source) + ".json_documents", "SHOW CREATE TABLE " + qi(f.source) + ".json_documents"}) != before {
		t.Fatal("failed JSON-check import wrote invalid rows or modified its source")
	}
	assertExecutionAccountLocked(t, x)
}
