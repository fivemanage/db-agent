package restoreplan

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const viewSession = "SET character_set_client=utf8mb4; SET character_set_results=utf8mb4; SET collation_connection=utf8mb4_bin;"
const simpleView = "CREATE ALGORITHM=UNDEFINED DEFINER='root'@'localhost' SQL SECURITY DEFINER VIEW original.balances AS SELECT original.players.id AS id, original.players.note AS note FROM original.players WHERE original.players.id >= 1;"

func TestViewIdentifiersDefinerAndDependenciesAreReconstructed(t *testing.T) {
	input := header + simpleTable + viewSession +
		"CREATE VIEW original.summary AS SELECT b.id AS player_id, b.note AS note FROM original.balances AS b WHERE b.id > 0;" + simpleView
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := []View{{Name: "balances", Columns: []string{"id", "note"}, Collation: "utf8mb4_bin"}, {Name: "summary", Columns: []string{"player_id", "note"}, Collation: "utf8mb4_bin"}}
	if !reflect.DeepEqual(plan.Views(), want) {
		t.Fatalf("view inventory/dependency order is incorrect: %+v", plan.Views())
	}
	output := strings.Join(statements(t, plan), ";\n")
	for _, fragment := range []string{"root", "original.players", "original.balances", "sql security invoker"} {
		if strings.Contains(output, fragment) {
			t.Fatal("view retained an original reference or privileged/invoker security context")
		}
	}
	if !strings.Contains(output, "sql security definer view "+testOptions().Destination+".balances") || !strings.Contains(output, testOptions().Destination+".players.id") || !strings.Contains(output, "collation_connection = utf8mb4_bin") {
		t.Fatal("view omitted destination identifiers, restricted definer binding, or source collation")
	}
	views := plan.Views()
	views[0].Columns[0] = "changed"
	if !reflect.DeepEqual(plan.Views(), want) {
		t.Fatal("caller mutated validated view metadata")
	}
}

func TestViewJoinPredicatesAndLiteralData(t *testing.T) {
	input := header + simpleTable + "CREATE TABLE items (id INT, owner INT) ENGINE=InnoDB;" + viewSession + `
CREATE VIEW joined AS SELECT p.id AS id, i.id AS item_id, p.id + 1 AS next_id,
 'original.players; # literal -- text' AS label
FROM original.players AS p LEFT JOIN original.items AS i ON p.id=i.owner
WHERE p.id>=1 AND (i.id IS NULL OR i.id>0);`
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Join(statements(t, plan), ";\n")
	if !strings.Contains(output, "'original.players; # literal -- text'") || !strings.Contains(output, "left join "+testOptions().Destination+".items") {
		t.Fatal("view literal was rewritten or join escaped the destination")
	}
}

func TestNativeViewPlaceholdersAreValidatedButNeverEmitted(t *testing.T) {
	for _, kind := range []string{"table", "view"} {
		t.Run(kind, func(t *testing.T) {
			prefix := "DROP TABLE IF EXISTS balances;"
			if kind == "table" {
				prefix += "CREATE TABLE balances (id INT, note TEXT); DROP TABLE IF EXISTS balances; DROP VIEW IF EXISTS balances;"
			} else {
				prefix += "DROP VIEW IF EXISTS balances; /*!50001 CREATE VIEW balances AS SELECT NULL AS id, NULL AS note */; DROP VIEW IF EXISTS balances;"
			}
			session := `USE original;
/*!50001 SET @saved_cs_client=@@character_set_client */;
/*!50001 SET @saved_cs_results=@@character_set_results */;
/*!50001 SET @saved_col_connection=@@collation_connection */;
/*!50001 SET character_set_client=utf8mb4 */;
/*!50001 SET character_set_results=utf8mb4 */;
/*!50001 SET collation_connection=utf8mb4_bin */;
/*!50001 CREATE ALGORITHM=UNDEFINED */
/*!50013 DEFINER='root'@'localhost' SQL SECURITY DEFINER */
/*!50001 VIEW balances AS SELECT players.id AS id, players.note AS note FROM original.players WHERE (players.id>=1) */;
/*!50001 SET character_set_client=@saved_cs_client */;
/*!50001 SET character_set_results=@saved_cs_results */;
/*!50001 SET collation_connection=@saved_col_connection */;
`
			plan, err := Prepare(context.Background(), strings.NewReader(header+simpleTable+prefix+session), testOptions())
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Tables()) != 1 || len(plan.Views()) != 1 {
				t.Fatal("native placeholder was counted as an actual object")
			}
			output := strings.Join(statements(t, plan), ";\n")
			for _, forbidden := range []string{"drop table", "drop view", "NULL as id", "create table " + testOptions().Destination + ".balances", "@saved"} {
				if strings.Contains(output, forbidden) {
					t.Fatal("native placeholder/bookkeeping entered the executable plan")
				}
			}
		})
	}
}

func TestUnsafeViewBodiesNeverProduceAPartialPlan(t *testing.T) {
	cases := map[string]string{
		"cross_schema":       "SELECT other.players.id AS id FROM other.players",
		"cross_column":       "SELECT other.players.id AS id FROM original.players",
		"system_schema":      "SELECT user AS id FROM mysql.user",
		"original_alias":     "SELECT original.players.id AS id FROM original.players AS p",
		"alias_escape":       "SELECT other.p.id AS id FROM original.players AS p",
		"unknown_relation":   "SELECT id AS id FROM missing",
		"unknown_column":     "SELECT missing AS id FROM players",
		"duplicate_alias":    "SELECT p.id AS id FROM players p JOIN players p ON p.id=p.id",
		"ambiguous_column":   "SELECT id AS id FROM players p JOIN players q ON p.id=q.id",
		"unresolved_alias":   "SELECT unknown.id AS id FROM players p",
		"udf":                "SELECT arbitrary_udf(id) AS id FROM players",
		"qualified_function": "SELECT original.balance(id) AS id FROM players",
		"advisory_lock":      "SELECT GET_LOCK('x',0) AS id FROM players",
		"side_effect_where":  "SELECT id FROM players WHERE SLEEP(1)=0",
		"current_user":       "SELECT CURRENT_USER() AS id FROM players",
		"session_variable":   "SELECT @private AS id FROM players",
		"assignment":         "SELECT @x:=id AS id FROM players",
		"file":               "SELECT id INTO OUTFILE '/outside' FROM players",
		"subquery":           "SELECT (SELECT id FROM players) AS id FROM players",
		"derived_table":      "SELECT q.id AS id FROM (SELECT id FROM players) q",
		"union":              "SELECT id FROM players UNION SELECT id FROM players",
		"star":               "SELECT * FROM players",
		"aggregate":          "SELECT COUNT(*) AS id FROM players",
		"locking":            "SELECT id FROM players FOR UPDATE",
		"order":              "SELECT id FROM players ORDER BY id",
		"limit":              "SELECT id FROM players LIMIT 1",
		"hint":               "SELECT /*+ SET_VAR(sql_mode='') */ id FROM players",
		"cte":                "WITH p AS (SELECT id FROM players) SELECT id FROM p",
		"natural_join":       "SELECT p.id FROM players p NATURAL JOIN players q",
		"missing_on":         "SELECT p.id FROM players p JOIN players q",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			input := header + simpleTable + viewSession + simpleView + "CREATE VIEW unsafe AS " + body + ";"
			plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
			if plan != nil || !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "/outside") || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe view returned a partial plan or sensitive diagnostics: %v", err)
			}
		})
	}
}

func TestViewSecurityPlaceholdersAndCyclesFailClosed(t *testing.T) {
	cases := []string{
		"CREATE SQL SECURITY INVOKER VIEW unsafe AS SELECT id FROM players;",
		"CREATE ALGORITHM=TEMPTABLE VIEW unsafe AS SELECT id FROM players;",
		"CREATE OR REPLACE VIEW unsafe AS SELECT id FROM players;",
		"CREATE VIEW unsafe AS SELECT id FROM players WITH CHECK OPTION;",
		"CREATE VIEW unsafe AS SELECT id FROM players; CREATE VIEW unsafe AS SELECT id FROM players;",
		"CREATE VIEW a AS SELECT id FROM b; CREATE VIEW b AS SELECT id FROM a;",
		"CREATE VIEW a AS SELECT id FROM a;",
		"CREATE VIEW a AS SELECT id FROM PLAYERS;",
		"DROP TABLE IF EXISTS dummy; CREATE TABLE dummy (id INT);",
		"DROP TABLE IF EXISTS dummy; CREATE TABLE dummy (id INT DEFAULT (GET_LOCK('x',0))); DROP TABLE IF EXISTS dummy; DROP VIEW IF EXISTS dummy; CREATE VIEW dummy AS SELECT id FROM players;",
		"DROP TABLE IF EXISTS dummy; CREATE TABLE dummy (different INT); DROP TABLE IF EXISTS dummy; DROP VIEW IF EXISTS dummy; CREATE VIEW dummy AS SELECT id FROM players;",
		"DROP TABLE IF EXISTS dummy; CREATE TABLE dummy (id INT); CREATE VIEW dummy AS SELECT id FROM players;",
		"DROP TABLE IF EXISTS dummy; DROP VIEW IF EXISTS dummy; CREATE VIEW dummy AS SELECT NULL AS id;",
		"DROP TABLE IF EXISTS dummy; DROP VIEW IF EXISTS dummy; CREATE VIEW dummy AS SELECT NULL AS id; INSERT INTO dummy VALUES (1); DROP VIEW IF EXISTS dummy; CREATE VIEW dummy AS SELECT id FROM players;",
	}
	for _, tail := range cases {
		plan, err := Prepare(context.Background(), strings.NewReader(header+simpleTable+viewSession+tail), testOptions())
		if plan != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsafe security context, placeholder sequence, or dependency returned a plan: %v", err)
		}
	}
	if plan, err := Prepare(context.Background(), strings.NewReader(header+simpleTable+simpleView), testOptions()); plan != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatal("view creation without captured source collation was accepted")
	}
	options := testOptions()
	options.MaxTables = 1
	if plan, err := Prepare(context.Background(), strings.NewReader(header+simpleTable+viewSession+simpleView), options); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("view objects bypassed the metadata resource bound")
	}
}

func TestViewExpressionDepthAndOutputExpansionAreBounded(t *testing.T) {
	input := header + simpleTable + viewSession + "CREATE VIEW deep AS SELECT " + strings.Repeat("id+", 200) + "id AS value FROM players;"
	if plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions()); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("left-deep expressions bypassed the preflight recursion bound")
	}
	// Repeated schema-qualified expressions grow when a short source schema is
	// rebuilt under a longer generated destination; the output budget is separate.
	var columns []string
	for i := 0; i < 26; i++ {
		columns = append(columns, "original.players.id AS "+string(rune('a'+i)))
	}
	input = header + simpleTable + viewSession + "CREATE VIEW expanded AS SELECT " + strings.Join(columns, ",") + " FROM original.players;"
	options := testOptions()
	options.MaxInputBytes = int64(len(input))
	if plan, err := Prepare(context.Background(), strings.NewReader(input), options); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("rebuilt view output bypassed the serialized plan budget")
	}
}
