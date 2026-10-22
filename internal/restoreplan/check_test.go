package restoreplan

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestJSONColumnChecksArePreserved(t *testing.T) {
	for _, driver := range []string{"mysql", "mariadb"} {
		for _, definition := range []string{
			"payload LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin CHECK (json_valid(`payload`))",
			"payload LONGTEXT, CONSTRAINT payload_json CHECK (JSON_VALID(payload))",
			"payload LONGTEXT, CHECK ((JSON_VALID((payload))))",
		} {
			t.Run(driver+"/"+definition, func(t *testing.T) {
				options := testOptions()
				options.Driver = driver
				input := header + "CREATE TABLE documents (id INT PRIMARY KEY, " + definition + ") ENGINE=InnoDB;" +
					`INSERT INTO documents VALUES (1,'{"original.players":"text"}');` +
					"CREATE TABLE children (id INT PRIMARY KEY, document_id INT, payload LONGTEXT, FOREIGN KEY (document_id) REFERENCES documents(id), CHECK (json_valid(payload))) ENGINE=InnoDB;" +
					"INSERT INTO children VALUES (1,1,NULL);"
				memory, err := Prepare(context.Background(), strings.NewReader(input), options)
				if err != nil {
					t.Fatal(err)
				}
				file, err := PrepareFile(context.Background(), privateSQLFile(t, input), options)
				if err != nil {
					t.Fatal(err)
				}
				output := statements(t, file)
				if !file.HasJSONChecks() || !memory.HasJSONChecks() || !reflect.DeepEqual(output, statements(t, memory)) || !reflect.DeepEqual(file.Tables(), []Table{{Name: "documents", Rows: 1}, {Name: "children", Rows: 1}}) {
					t.Fatal("JSON checks changed the file-backed plan, row inventory, or FK ordering")
				}
				joined := strings.Join(output, ";\n")
				preservedJSON := strings.Contains(joined, `'{\"original.players\":\"text\"}'`) || strings.Contains(joined, `'{"original.players":"text"}'`)
				if strings.Count(joined, "check (json_valid(payload))") != 2 || !preservedJSON {
					t.Fatal("JSON check or literal data was lost during reconstruction")
				}
				if strings.Contains(definition, "CONSTRAINT payload_json") && !strings.Contains(joined, "constraint payload_json check") {
					t.Fatal("named JSON check was not preserved")
				}
				if got := strings.Contains(joined, "SET SESSION check_constraint_checks = 1"); got != (driver == "mariadb") {
					t.Fatal("CHECK enforcement was not pinned for the correct engine")
				}
			})
		}
	}
}

func TestUnsafeOrUnsupportedChecksRejectTheWholePlan(t *testing.T) {
	for _, expression := range []string{
		"json_valid(missing)",
		"json_valid(original.documents.payload)",
		"json_valid(other.documents.payload)",
		"json_valid(documents.payload)",
		"json_valid(original.json_valid(payload))",
		"original.json_valid(payload)",
		"json_valid(GET_LOCK('private',0))",
		"json_valid((SELECT payload FROM other.documents LIMIT 1))",
		"json_valid(@private)",
		"json_valid(CONCAT(payload,'private'))",
		"json_valid('private')",
		"json_valid(payload) OR GET_LOCK('private',0)",
		"json_valid(payload) OR 1",
		"json_valid(payload) = 1",
		"json_type(payload)",
		"json_depth(payload)",
		"id > 0",
		"GET_LOCK('private',0)",
	} {
		t.Run(expression, func(t *testing.T) {
			input := header + simpleTable + "INSERT INTO players VALUES (1,'accepted prefix',NULL);" +
				"CREATE TABLE documents (id INT, payload LONGTEXT, CHECK (" + expression + ")) ENGINE=InnoDB;"
			for _, fileBacked := range []bool{false, true} {
				var plan *Plan
				var err error
				if fileBacked {
					plan, err = PrepareFile(context.Background(), privateSQLFile(t, input), testOptions())
				} else {
					plan, err = Prepare(context.Background(), strings.NewReader(input), testOptions())
				}
				if plan != nil || !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "private") {
					t.Fatalf("unsupported check produced a plan or disclosed SQL: %v", err)
				}
			}
		})
	}
	input := header + "CREATE TABLE documents (payload LONGTEXT, CHECK (json_valid(payload)) NOT ENFORCED) ENGINE=InnoDB;"
	if plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions()); plan != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatal("unenforced JSON check was accepted")
	}
}
