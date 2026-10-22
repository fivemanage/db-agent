package restoreplan

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestFulltextIndexIsPreserved(t *testing.T) {
	input := header + "CREATE TABLE articles (id INT PRIMARY KEY, title VARCHAR(255), summary TEXT, content LONGTEXT, FULLTEXT KEY idx_search (title, summary, content)) ENGINE=InnoDB;" +
		"INSERT INTO articles VALUES (1,'original.players','searchable summary','content');"
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	file, err := PrepareFile(context.Background(), privateSQLFile(t, input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	output := statements(t, file)
	if !reflect.DeepEqual(output, statements(t, plan)) || !strings.Contains(strings.Join(output, ";\n"), "fulltext key idx_search (title, summary, content)") {
		t.Fatal("FULLTEXT definition was lost or changed by the file-backed plan")
	}
}

func TestUnsupportedFulltextOptionsRejectTheWholePlan(t *testing.T) {
	for _, index := range []string{
		"FULLTEXT KEY idx_search (content) WITH PARSER private_plugin",
		"FULLTEXT KEY idx_search (content) USING BTREE",
		"FULLTEXT KEY idx_search (content(10))",
		"FULLTEXT KEY idx_search (id)",
		"FULLTEXT KEY idx_search (missing)",
		"FULLTEXT KEY idx_search ((GET_LOCK('private',0)))",
		"SPATIAL KEY idx_search (content)",
	} {
		t.Run(index, func(t *testing.T) {
			input := header + simpleTable + "INSERT INTO players VALUES (1,'accepted prefix',NULL);" +
				"CREATE TABLE articles (id INT, content TEXT, " + index + ") ENGINE=InnoDB;"
			plan, err := PrepareFile(context.Background(), privateSQLFile(t, input), testOptions())
			if plan != nil || !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsupported FULLTEXT definition returned a plan or disclosed SQL: %v", err)
			}
		})
	}
}
