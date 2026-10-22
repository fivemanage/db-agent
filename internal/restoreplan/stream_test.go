package restoreplan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func privateSQLFile(t *testing.T, sql string) *os.File {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(t.TempDir(), "verified.sql"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if _, err := file.WriteString(sql); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestFilePlanMatchesMemoryPlanAndPreservesDependencyOrder(t *testing.T) {
	input := header + `
CREATE TABLE items (id INT PRIMARY KEY, owner INT, FOREIGN KEY (owner) REFERENCES players(id)) ENGINE=InnoDB;
INSERT INTO items VALUES (1,1);
` + simpleTable + `
# comment with a ; and 'quote'
/* comment containing ; and "quotes" */
INSERT INTO players VALUES (1,'quote\' doubled'' ; /* -- \\ 🙂',0x00FF);
` + viewSession + simpleView
	memory, err := Prepare(context.Background(), iotest.OneByteReader(strings.NewReader(input)), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	file := privateSQLFile(t, input)
	disk, err := PrepareFile(context.Background(), file, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(statements(t, memory), statements(t, disk)) || !reflect.DeepEqual(memory.Tables(), disk.Tables()) || !reflect.DeepEqual(memory.Views(), disk.Views()) {
		t.Fatal("file-backed plan changed SQL, dependency order, or validation inventory")
	}
	for _, statement := range disk.statements {
		if strings.HasPrefix(statement.sql, "insert") {
			t.Fatal("file-backed plan retained row SQL in memory")
		}
	}
	if _, err := file.WriteAt([]byte("X"), int64(strings.Index(input, "quote\\'"))); err != nil {
		t.Fatal(err)
	}
	if err := disk.ForEach(context.Background(), func(string) error { return nil }); !errors.Is(err, ErrInput) {
		t.Fatal("changed row SQL was consumed without matching its validated hash")
	}
}

func TestFilePlanLargerThanOldLimit(t *testing.T) {
	minimum := int64(65 << 20)
	if os.Getenv("DB_AGENT_LARGE_RESTORE_TEST") == "1" {
		minimum = 2<<30 + 1
	}
	file := privateSQLFile(t, header+simpleTable)
	payload := strings.Repeat("x", 512<<10)
	var rows int64
	for {
		position, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			t.Fatal(err)
		}
		if position >= minimum {
			t.Logf("SQL preflight fixture: %d bytes, %d rows", position, rows)
			break
		}
		rows++
		if _, err := fmt.Fprintf(file, "INSERT INTO players VALUES (%d,'%s',NULL);\n", rows, payload); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PrepareFile(context.Background(), file, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Tables(); len(got) != 1 || got[0].Rows != rows {
		t.Fatal("large plan lost row validation metadata")
	}
	var inserts int64
	if err := plan.ForEach(context.Background(), func(sql string) error {
		if strings.HasPrefix(sql, "insert") {
			inserts++
			if !strings.Contains(sql, testOptions().Destination+".players") {
				t.Fatal("large insert escaped the destination")
			}
		}
		return nil
	}); err != nil || inserts != rows {
		t.Fatalf("large plan consumption: inserts=%d, error=%v", inserts, err)
	}
	for _, statement := range plan.statements {
		if statement.size != 0 && statement.sql != "" {
			t.Fatal("row references retained SQL text in memory")
		}
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("UPDATE original.players SET note='private';"); err != nil {
		t.Fatal(err)
	}
	if partial, err := PrepareFile(context.Background(), file, testOptions()); partial != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("large supported prefix bypassed trailing SQL rejection: %v", err)
	}
}

func TestStreamingBoundsAndTrailingRejection(t *testing.T) {
	input := header + simpleTable + "INSERT INTO players VALUES (1,'" + strings.Repeat("a", 1024) + "',NULL);"
	for _, tail := range []string{
		"UPDATE original.players SET note='private';",
		"/*!999999 UPDATE original.players SET note='private' */;",
		"/*M!100100 UPDATE original.players SET note='private' */;",
		"/* comment */ INSERT INTO players VALUES (2,'private',NULL)",
		"\xff", "\x00", "/* unfinished", "-- comment\nSELECT 'unfinished",
	} {
		plan, err := PrepareFile(context.Background(), privateSQLFile(t, input+tail), testOptions())
		if plan != nil || !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe trailing input returned a plan or disclosed SQL: %v", err)
		}
	}
	for _, mutate := range []func(*Options){
		func(o *Options) { o.MaxInputBytes = int64(len(input) - 1) },
		func(o *Options) { o.MaxStatementBytes = 10 },
		func(o *Options) { o.MaxStatements = 3 },
	} {
		options := testOptions()
		mutate(&options)
		if plan, err := PrepareFile(context.Background(), privateSQLFile(t, input), options); plan != nil || !errors.Is(err, ErrLimit) {
			t.Fatalf("file-backed preflight bypassed a resource bound: %v", err)
		}
	}
	options := testOptions()
	options.MaxInputBytes = int64(len(input))
	if _, err := PrepareFile(context.Background(), privateSQLFile(t, input), options); err != nil {
		t.Fatalf("exact input limit rejected: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if plan, err := PrepareFile(ctx, privateSQLFile(t, input), testOptions()); plan != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("file-backed preflight ignored cancellation")
	}
}

func FuzzStreamingFraming(f *testing.F) {
	for _, input := range []string{
		header + simpleTable,
		"SELECT 'a'';b'; SELECT \"c\\\";d\";",
		"/* ; */ /*/'*/ SELECT 1; -- ';\nSELECT `a``;b`;",
		"/*!50000 SELECT 1; SELECT 2 */;",
		"/*M!999999\\- enable sandbox mode */\n" + header,
	} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 4096 {
			return
		}
		options := testOptions()
		if err := options.defaults(); err != nil {
			t.Fatal(err)
		}
		want, wantErr := split(context.Background(), input, options)
		var got []string
		err := scanStatements(context.Background(), iotest.OneByteReader(strings.NewReader(input)), options, func(raw string, _ int64) error {
			parts, err := split(context.Background(), raw, options)
			if err == nil {
				got = append(got, parts...)
			}
			return err
		})
		if err == nil && (wantErr != nil || !reflect.DeepEqual(got, want)) {
			t.Fatal("streaming framing accepted or changed SQL rejected by whole-input framing")
		}
		if err != nil && wantErr == nil {
			t.Fatalf("streaming framing rejected supported SQL: %v", err)
		}
	})
}
