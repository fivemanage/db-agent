package restoreplan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const triggerSession = viewSession + "SET @saved_sql_mode=@@sql_mode; SET sql_mode='STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION';"
const triggerTables = simpleTable + "CREATE TABLE audit_log (player_id INT, action TEXT) ENGINE=InnoDB;"
const auditTrigger = "CREATE DEFINER='root'@'localhost' TRIGGER original.record_insert AFTER INSERT ON original.players FOR EACH ROW INSERT INTO original.audit_log VALUES (NEW.id, 'original.players; literal');"

func TestTriggerDefinersReferencesAndLoadOrderAreReconstructed(t *testing.T) {
	input := header + triggerTables + triggerSession + auditTrigger +
		"INSERT INTO players VALUES (1,'original.players',NULL);" + simpleView +
		"CREATE TRIGGER second AFTER INSERT ON players FOR EACH ROW INSERT INTO audit_log (action,player_id) VALUES ('second',NEW.id);"
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := []Trigger{
		{Name: "record_insert", Table: "players", Timing: "AFTER", Event: "INSERT", Collation: "utf8mb4_bin", SQLMode: "STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION", Order: 1},
		{Name: "second", Table: "players", Timing: "AFTER", Event: "INSERT", Collation: "utf8mb4_bin", SQLMode: "STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION", Order: 2},
	}
	if !reflect.DeepEqual(plan.Triggers(), want) {
		t.Fatalf("incorrect trigger inventory/order: %+v", plan.Triggers())
	}
	output := strings.Join(statements(t, plan), ";\n")
	for _, forbidden := range []string{"root", "original.audit_log", "on original.players", "trigger original."} {
		if strings.Contains(output, forbidden) {
			t.Fatal("trigger retained privileged identity or original schema reference")
		}
	}
	triggerAt := strings.Index(output, "CREATE TRIGGER ")
	if triggerAt < strings.Index(output, "insert into "+testOptions().Destination+".players") ||
		triggerAt < strings.Index(output, "sql security definer view") ||
		!strings.Contains(output, "'original.players; literal'") ||
		!strings.Contains(output, "insert into "+testOptions().Destination+".audit_log") {
		t.Fatal("trigger was created before data/views, omitted its target, or changed literal data")
	}
	inventory := plan.Triggers()
	inventory[0].Name = "changed"
	if !reflect.DeepEqual(plan.Triggers(), want) {
		t.Fatal("caller changed the validated trigger inventory")
	}
}

func TestTriggerBlocksConditionsAndRowAssignments(t *testing.T) {
	input := header + triggerTables + triggerSession + `
DELIMITER ;;
CREATE TRIGGER normalize BEFORE UPDATE ON players FOR EACH ROW
BEGIN
 IF new.id > old.id THEN
  SET NEW.note = OLD.note, NEW.id=NEW.id+1;
 ELSEIF NEW.id = OLD.id THEN
  BEGIN SET NEW.note = 'same; END IF;'; END;
 ELSE
  SET NEW.note = 'lower';
 END IF;
 INSERT INTO original.audit_log (player_id,action) VALUES (NEW.id,NEW.note);
 UPDATE original.audit_log SET action=NEW.note WHERE player_id=OLD.id;
 DELETE FROM original.audit_log WHERE player_id<OLD.id;
END;;
DELIMITER ;
`
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Join(statements(t, plan), ";\n")
	for _, fragment := range []string{"BEGIN IF NEW.id > OLD.id THEN", "SET NEW.note = OLD.note, NEW.id = NEW.id + 1",
		"ELSEIF NEW.id = OLD.id THEN", "'same; END IF;'", "update " + testOptions().Destination + ".audit_log",
		"delete from " + testOptions().Destination + ".audit_log"} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("trigger body omitted validated structure: %s", fragment)
		}
	}
}

func TestNativeTriggerExecutableCommentsAndStreamedPlans(t *testing.T) {
	for _, delimiter := range []string{";;", "$$", "//"} {
		t.Run(delimiter, func(t *testing.T) {
			input := header + triggerTables + triggerSession + "\nDELIMITER " + delimiter + `
/*!50032 DROP TRIGGER IF EXISTS original.record_insert */` + delimiter + `
/*!50003 CREATE*/ /*!50017 DEFINER='root'@'localhost'*/ /*!50003 TRIGGER original.record_insert AFTER INSERT ON players FOR EACH ROW
BEGIN
 INSERT INTO audit_log VALUES (NEW.id,'data; $$ //');
END */` + delimiter + `
DELIMITER ;
SET sql_mode=@saved_sql_mode;
INSERT INTO players VALUES (1,'after trigger',NULL);`
			path := filepath.Join(t.TempDir(), "verified.sql")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			streamed, err := PrepareFile(context.Background(), file, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			memory, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(statements(t, streamed), statements(t, memory)) || len(streamed.Triggers()) != 1 {
				t.Fatal("streamed preflight changed native trigger framing or retained row offsets")
			}
		})
	}
}

func TestUnsupportedTriggerSQLNeverReturnsAPartialPlan(t *testing.T) {
	cases := map[string]string{
		"outside_trigger":   "CREATE TRIGGER other.bad AFTER INSERT ON players FOR EACH ROW SET NEW.id=1;",
		"outside_table":     "CREATE TRIGGER bad AFTER INSERT ON other.players FOR EACH ROW INSERT INTO audit_log VALUES (NEW.id,'x');",
		"outside_insert":    "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW INSERT INTO other.audit_log VALUES (NEW.id,'x');",
		"outside_update":    "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW UPDATE other.audit_log SET action='x';",
		"outside_delete":    "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW DELETE FROM other.audit_log;",
		"outside_column":    "CREATE TRIGGER bad AFTER UPDATE ON players FOR EACH ROW UPDATE audit_log SET action=other.audit_log.action;",
		"schema_pseudorow":  "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW INSERT INTO audit_log VALUES (original.NEW.id,'x');",
		"old_insert":        "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW INSERT INTO audit_log VALUES (OLD.id,'x');",
		"new_delete":        "CREATE TRIGGER bad AFTER DELETE ON players FOR EACH ROW INSERT INTO audit_log VALUES (NEW.id,'x');",
		"after_assignment":  "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW SET NEW.id=1;",
		"old_assignment":    "CREATE TRIGGER bad BEFORE UPDATE ON players FOR EACH ROW SET OLD.id=1;",
		"self_write":        "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW UPDATE players SET id=1;",
		"missing_table":     "CREATE TRIGGER bad AFTER INSERT ON missing FOR EACH ROW INSERT INTO audit_log VALUES (NEW.id,'x');",
		"missing_target":    "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW DELETE FROM missing;",
		"missing_column":    "CREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW SET NEW.missing=1;",
		"bad_target_column": "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW UPDATE audit_log SET missing=1;",
		"unqualified_row":   "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW INSERT INTO audit_log VALUES (id,'x');",
		"bad_width":         "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW INSERT INTO audit_log VALUES (NEW.id);",
		"insert_select":     "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW INSERT INTO audit_log SELECT id,note FROM players;",
		"insert_ignore":     "CREATE TRIGGER bad AFTER INSERT ON players FOR EACH ROW INSERT IGNORE INTO audit_log VALUES (NEW.id,'x');",
		"udf":               "CREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW SET NEW.id=arbitrary_udf(NEW.id);",
		"function_schema":   "CREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW SET NEW.id=original.balance(NEW.id);",
		"subquery":          "CREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW SET NEW.id=(SELECT id FROM other.players);",
		"variable":          "CREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW SET @private=NEW.id;",
		"drop_only":         "DROP TRIGGER IF EXISTS missing;",
		"duplicate":         auditTrigger,
		"drop_created":      "DROP TRIGGER IF EXISTS record_insert;",
		"if_missing_end":    "\nDELIMITER ;;\nCREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW BEGIN IF NEW.id>0 THEN SET NEW.id=1; END;;\nDELIMITER ;\n",
		"escape_block":      "\nDELIMITER ;;\nCREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW BEGIN SET NEW.id=1; END; DELETE FROM other.players;;\nDELIMITER ;\n",
		"hidden_statement":  "\nDELIMITER ;;\n/*!50003 CREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW SET NEW.id=1; DELETE FROM other.players */;;\nDELIMITER ;\n",
		"dynamic":           "\nDELIMITER ;;\nCREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW BEGIN PREPARE stmt FROM 'DELETE FROM other.players'; END;;\nDELIMITER ;\n",
		"client_command":    "\nDELIMITER ;;\nCREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW BEGIN \\! touch /private; END;;\nDELIMITER ;\n",
		"mode":              "SET sql_mode='NO_BACKSLASH_ESCAPES';",
		"bad_definer":       "CREATE DEFINER='root'@'localhost' SQL SECURITY DEFINER TRIGGER bad BEFORE INSERT ON players FOR EACH ROW SET NEW.id=1;",
	}
	for name, tail := range cases {
		t.Run(name, func(t *testing.T) {
			plan, err := Prepare(context.Background(), strings.NewReader(header+triggerTables+triggerSession+auditTrigger+tail), testOptions())
			if plan != nil || !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "root") {
				t.Fatalf("unsafe trigger returned an executable/partial plan or sensitive diagnostic: %v", err)
			}
		})
	}
}

func TestTriggerLimitsAndMissingCreationContext(t *testing.T) {
	for _, session := range []string{"", viewSession, "SET sql_mode='';"} {
		plan, err := Prepare(context.Background(), strings.NewReader(header+triggerTables+session+auditTrigger), testOptions())
		if plan != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatal("trigger without captured SQL mode/collation returned a plan")
		}
	}
	options := testOptions()
	options.MaxTables = 2
	if plan, err := Prepare(context.Background(), strings.NewReader(header+triggerTables+triggerSession+auditTrigger), options); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("trigger bypassed object inventory limits")
	}
	nested := header + triggerTables + triggerSession + "\nDELIMITER ;;\nCREATE TRIGGER deep BEFORE INSERT ON players FOR EACH ROW " +
		strings.Repeat("BEGIN ", 34) + "SET NEW.id=1; " + strings.Repeat("END; ", 33) + "END;;\nDELIMITER ;\n"
	if plan, err := Prepare(context.Background(), strings.NewReader(nested), testOptions()); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("trigger nesting bypassed bounded preflight: %v", err)
	}
}

func TestTriggerTimestampExpressionsAndQuotedNames(t *testing.T) {
	input := header + triggerTables + triggerSession +
		"CREATE\nTRIGGER `record``insert` BEFORE INSERT ON players FOR EACH ROW SET\nNEW.note=CONCAT('created ',CURRENT_TIMESTAMP(6));"
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Join(statements(t, plan), ";\n")
	if plan.Triggers()[0].Name != "record`insert" || !strings.Contains(output, ".`record``insert`") || !strings.Contains(output, "current_timestamp(6)") {
		t.Fatal("quoted trigger name or timestamp expression was not reconstructed")
	}
}

func TestMalformedDelimiterSequencesAreRejected(t *testing.T) {
	for _, tail := range []string{
		"\nDELIMITER ;;\n" + auditTrigger + "\nDELIMITER ;\n",
		"\nDELIMITER arbitrary\n",
		"\nDELIMITER ;;\n",
		"\nDELIMITER ;;\nDELIMITER $$\n",
		"\nDELIMITER ;;\nINSERT INTO players VALUES (1,'x',NULL);;\nDELIMITER ;\n",
		"\nDELIMITER ;;\nCREATE TRIGGER bad BEFORE INSERT ON players FOR EACH ROW BEGIN SET NEW.id=1; END",
	} {
		plan, err := Prepare(context.Background(), strings.NewReader(header+triggerTables+triggerSession+tail), testOptions())
		if plan != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatalf("malformed delimiter sequence returned a plan: %v", err)
		}
	}
}

func TestTriggerStatementAndOutputBudgets(t *testing.T) {
	input := header + triggerTables + triggerSession +
		"\nDELIMITER ;;\nCREATE TRIGGER oversized BEFORE INSERT ON players FOR EACH ROW BEGIN " +
		strings.Repeat("SET NEW.id=1; ", 1025) + "END;;\nDELIMITER ;\n"
	if plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions()); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("trigger bypassed its body statement budget: %v", err)
	}
	input = header + strings.ReplaceAll(triggerTables, "audit_log", "a") + triggerSession +
		"\nDELIMITER ;;\nCREATE TRIGGER expanded AFTER INSERT ON players FOR EACH ROW BEGIN " +
		strings.Repeat("INSERT INTO a VALUES (NEW.id,'x'); ", 100) + "END;;\nDELIMITER ;\n"
	options := testOptions()
	options.MaxInputBytes = int64(len(input))
	if plan, err := Prepare(context.Background(), strings.NewReader(input), options); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("expanded trigger bypassed serialized plan budget: %v", err)
	}
}
