package restoreplan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func testOptions() Options {
	return Options{Driver: "mysql", Source: "original", Destination: "fm_restore_" + strings.Repeat("a", 32)}
}

const header = "CREATE DATABASE IF NOT EXISTS original DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; USE original;"
const simpleTable = "CREATE TABLE original.players (id INT PRIMARY KEY, note TEXT, payload BLOB) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;"

func statements(t *testing.T, plan *Plan) []string {
	t.Helper()
	var result []string
	if err := plan.ForEach(context.Background(), func(sql string) error { result = append(result, sql); return nil }); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPreflightRebuildsIdentifiersAndOrdersForeignKeys(t *testing.T) {
	input := header + `
DROP TABLE IF EXISTS items;
CREATE TABLE items (id INT PRIMARY KEY, owner INT, CONSTRAINT item_owner FOREIGN KEY (owner) REFERENCES original.players(id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO original.items VALUES (1, 1);
DROP TABLE IF EXISTS original.players;
CREATE TABLE original.players (id INT PRIMARY KEY, note TEXT, payload BLOB) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO players VALUES (1, 'original.players; USE original; /* data */', 0x0001FEFF);
`
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Tables(); !reflect.DeepEqual(got, []Table{{Name: "players", Rows: 1}, {Name: "items", Rows: 1}}) {
		t.Fatalf("wrong destination inventory/order: %+v", got)
	}
	output := statements(t, plan)
	if len(output) != 10 {
		t.Fatalf("unexpected statements: %d", len(output))
	}
	joined := strings.Join(output, ";\n")
	for _, forbidden := range []string{"drop table", "if not exists", "use original", "foreign_key_checks = 0", "unique_checks = 0"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unsafe/unnecessary dump statement was emitted: %s", forbidden)
		}
	}
	if !strings.Contains(joined, "'original.players; USE original; /* data */'") || !strings.Contains(strings.ToLower(joined), "0x0001feff") {
		t.Fatal("schema-like string or binary data was changed")
	}
	if !strings.Contains(output[6], testOptions().Destination+".players") || !strings.Contains(output[7], "references "+testOptions().Destination+".players") || !strings.Contains(output[8], ".players") || !strings.Contains(output[9], ".items") {
		t.Fatal("schema references or dependency order were not reconstructed")
	}
	inventory := plan.Tables()
	inventory[0].Name = "changed"
	if plan.Tables()[0].Name != "players" {
		t.Fatal("caller changed validated plan metadata")
	}
}

func TestNativeBookkeepingIsRecognizedButNotExecuted(t *testing.T) {
	input := `
/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40101 SET @OLD_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS */;
/*!40101 SET @OLD_COLLATION_CONNECTION=@@COLLATION_CONNECTION */;
/*!50503 SET NAMES utf8mb4 */;
/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;
/*!40103 SET TIME_ZONE='+00:00' */;
/*!40014 SET @OLD_UNIQUE_CHECKS=@@UNIQUE_CHECKS, UNIQUE_CHECKS=0 */;
/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */;
/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;
/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */;
/*M!100616 SET @OLD_NOTE_VERBOSITY=@@NOTE_VERBOSITY, NOTE_VERBOSITY=0 */;
` + header + `
DROP TABLE IF EXISTS players;
/*!40101 SET @saved_cs_client = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
` + simpleTable + `
/*!40101 SET character_set_client = @saved_cs_client */;
LOCK TABLES players WRITE;
/*!40000 ALTER TABLE players DISABLE KEYS */;
INSERT INTO players VALUES (1, 'private fixture', NULL);
/*!40000 ALTER TABLE players ENABLE KEYS */;
UNLOCK TABLES;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;
/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
/*M!100616 SET NOTE_VERBOSITY=@OLD_NOTE_VERBOSITY */;
`
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Join(statements(t, plan), ";\n")
	for _, forbidden := range []string{"@OLD", "foreign_key_checks = 0", "unique_checks = 0", "LOCK TABLES", "DISABLE KEYS", "saved_cs_client"} {
		if strings.Contains(output, forbidden) {
			t.Fatal("original session/locking directives entered the execution plan")
		}
	}
	if !strings.Contains(output, "STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION") {
		t.Fatal("plan omitted its pinned safe SQL mode")
	}
}

func TestQuotedLiteralsAndNamesRemainData(t *testing.T) {
	input := header + "CREATE TABLE `order` (`select` INT PRIMARY KEY, note TEXT, payload BLOB) ENGINE=InnoDB;" +
		"INSERT INTO `order` VALUES (-1, " + `'quote\' \\ doubled'' ; # -- /*!50000 DROP */', X'00FF');`
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Join(statements(t, plan), ";\n")
	if !strings.Contains(output, ".`order`") || !strings.Contains(output, "`select`") || plan.Tables()[0].Rows != 1 {
		t.Fatal("quoted identifiers or string boundaries were misinterpreted")
	}
}

func TestTypicalGameSchemaDefaults(t *testing.T) {
	input := header + "CREATE TABLE profiles (id INT AUTO_INCREMENT PRIMARY KEY, role ENUM('user','admin') NOT NULL DEFAULT 'user', balance DECIMAL(12,2) NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, note LONGTEXT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;"
	if _, err := Prepare(context.Background(), strings.NewReader(input), testOptions()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeMySQLDatabaseEncryptionOption(t *testing.T) {
	input := strings.Replace(header, "; USE", " /*!80016 DEFAULT ENCRYPTION='N' */; USE", 1) + simpleTable
	if _, err := Prepare(context.Background(), strings.NewReader(input), testOptions()); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedConstructsNeverProducePartialPlans(t *testing.T) {
	cases := map[string]string{
		"other_schema_insert":  "INSERT INTO other.players VALUES (1, 'secret', NULL);",
		"source_update":        "UPDATE original.players SET note = 'secret';",
		"insert_select":        "INSERT INTO players SELECT * FROM other.players;",
		"insert_function":      "INSERT INTO players VALUES (GET_LOCK('x',0), 'secret', NULL);",
		"insert_subquery":      "INSERT INTO players VALUES ((SELECT 1), 'secret', NULL);",
		"insert_default":       "INSERT INTO players VALUES (DEFAULT, 'secret', NULL);",
		"insert_ignore":        "INSERT IGNORE INTO players VALUES (1, 'secret', NULL);",
		"insert_duplicate":     "INSERT INTO players VALUES (1, 'secret', NULL) ON DUPLICATE KEY UPDATE note='secret';",
		"insert_wrong_columns": "INSERT INTO players (note,id,payload) VALUES ('secret', 1, NULL);",
		"replace":              "REPLACE INTO players VALUES (1, 'secret', NULL);",
		"view":                 "CREATE VIEW balances AS SELECT * FROM original.players;",
		"privileged_view":      "CREATE DEFINER='root'@'localhost' VIEW balances AS SELECT * FROM original.players;",
		"routine":              "CREATE PROCEDURE unsafe() UPDATE other.players SET id=0;",
		"trigger":              "CREATE TRIGGER unsafe AFTER INSERT ON players FOR EACH ROW SET @x=1;",
		"event":                "CREATE EVENT unsafe ON SCHEDULE EVERY 1 DAY DO SELECT 1;",
		"dynamic":              "PREPARE unsafe FROM 'UPDATE original.players SET id=0';",
		"delimiter":            "DELIMITER ;;\nCREATE PROCEDURE unsafe() BEGIN SELECT 1; END;;",
		"client":               "\\! echo secret;",
		"local_file":           "LOAD DATA LOCAL INFILE '/private/key' INTO TABLE players;",
		"global":               "SET GLOBAL max_connections=1;",
		"sql_mode":             "SET SQL_MODE='NO_BACKSLASH_ESCAPES';",
		"set_function":         "SET @OLD_SQL_MODE=GET_LOCK('x',0);",
		"unknown_user_var":     "SET @private = 'secret';",
		"user":                 "CREATE USER 'attacker'@'localhost';",
		"drop_after_create":    "DROP TABLE IF EXISTS original.players;",
		"drop_database":        "DROP DATABASE original;",
		"second_schema":        "CREATE DATABASE other; USE other;",
		"other_use":            "USE other;",
		"unknown_alter":        "ALTER TABLE players ADD COLUMN extra INT;",
		"conditional_future":   "/*!999999 UPDATE original.players SET note='secret' */;",
		"mariadb_directive":    "/*M!100100 UPDATE original.players SET note='secret' */;",
		"conditional_multiple": "/*!50000 SET NAMES utf8mb4; UPDATE original.players SET note='secret' */;",
		"optimizer_hint":       "INSERT /*+ SET_VAR(sql_mode='') */ INTO players VALUES (1, 'secret', NULL);",
		"nested_comment":       "/* outer /* hidden */ INSERT INTO players VALUES (1, 'secret', NULL);",
		"unfinished":           "INSERT INTO players VALUES (1, 'secret', NULL)",
		"unfinished_quote":     "INSERT INTO players VALUES (1, 'secret, NULL);",
		"invalid_utf8":         string([]byte{0xff}),
		"raw_nul":              "\x00",
	}
	for name, tail := range cases {
		t.Run(name, func(t *testing.T) {
			input := header + simpleTable + "INSERT INTO players VALUES (1,'accepted prefix',NULL);" + tail
			plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
			if plan != nil || !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "root") {
				t.Fatalf("unsupported trailing SQL returned a plan or unsafe diagnostic: %v", err)
			}
		})
	}
}

func TestUnsupportedTableDefinitionsAndDependencies(t *testing.T) {
	cases := []string{
		"CREATE TABLE players (id INT) ENGINE=MyISAM;",
		"CREATE TABLE players (id INT);",
		"CREATE TABLE players (id INT) ENGINE=InnoDB DATA DIRECTORY='/outside';",
		"CREATE TABLE players (id INT) ENGINE=FEDERATED CONNECTION='private';",
		"CREATE TABLE players (id INT) ENGINE=InnoDB TABLESPACE outside;",
		"CREATE TABLE players (id INT) ENGINE=InnoDB PARTITION BY HASH(id) PARTITIONS 2;",
		"CREATE TABLE players (id INT DEFAULT (GET_LOCK('x',0))) ENGINE=InnoDB;",
		"CREATE TABLE players (id INT, calc INT AS (id+1)) ENGINE=InnoDB;",
		"CREATE TABLE players (id INT, CHECK (GET_LOCK('x',0))) ENGINE=InnoDB;",
		"CREATE TABLE players (id INT INVISIBLE) ENGINE=InnoDB;",
		"CREATE TABLE IF NOT EXISTS players (id INT) ENGINE=InnoDB;",
		"CREATE TEMPORARY TABLE players (id INT) ENGINE=InnoDB;",
		"CREATE TABLE players LIKE original.other;",
		"CREATE TABLE players (id INT) ENGINE=InnoDB; CREATE TABLE PLAYERS (id INT) ENGINE=InnoDB;",
		"CREATE TABLE players (id INT, FOREIGN KEY (id) REFERENCES other.players(id)) ENGINE=InnoDB;",
		"CREATE TABLE players (id INT, FOREIGN KEY (id) REFERENCES missing(id)) ENGINE=InnoDB;",
		"CREATE TABLE players (id INT, FOREIGN KEY (id) REFERENCES players(id)) ENGINE=InnoDB;",
		"CREATE TABLE players (id INT, FOREIGN KEY (id) REFERENCES missing_parent(missing_column)) ENGINE=InnoDB; CREATE TABLE missing_parent (id INT) ENGINE=InnoDB;",
		"CREATE TABLE players (id INT, FOREIGN KEY (id) REFERENCES PARENT(id)) ENGINE=InnoDB; CREATE TABLE parent (id INT) ENGINE=InnoDB;",
		"CREATE TABLE a (id INT, FOREIGN KEY (id) REFERENCES b(id)) ENGINE=InnoDB; CREATE TABLE b (id INT, FOREIGN KEY (id) REFERENCES a(id)) ENGINE=InnoDB;",
	}
	for _, input := range cases {
		plan, err := Prepare(context.Background(), strings.NewReader(header+input), testOptions())
		if plan != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported table definition returned a plan: %v", err)
		}
	}
}

func TestSchemaBindingAndOptionsFailClosed(t *testing.T) {
	for _, prefix := range []string{
		"", "USE original;", "CREATE DATABASE original CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;",
		"CREATE DATABASE other CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; USE other;",
		"CREATE DATABASE original CHARACTER SET utf8mb4; USE original;",
		"CREATE DATABASE original CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COLLATE utf8mb4_general_ci; USE original;",
		"CREATE DATABASE original CHARACTER SET utf8mb4 COLLATE utf8mb4_bin ENCRYPTION='Y'; USE original;",
	} {
		if plan, err := Prepare(context.Background(), strings.NewReader(prefix+simpleTable), testOptions()); plan != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unbound/ambiguous schema produced a plan: %v", err)
		}
	}
	for _, mutate := range []func(*Options){
		func(o *Options) { o.Driver = "postgres" },
		func(o *Options) { o.Source = "mysql" },
		func(o *Options) { o.Source = "" },
		func(o *Options) { o.Source = o.Destination },
		func(o *Options) { o.Destination = "original" },
		func(o *Options) { o.MaxInputBytes = -1 },
		func(o *Options) { o.MaxInputBytes = 1<<30 + 1 },
		func(o *Options) { o.MaxStatementBytes = -1 },
		func(o *Options) { o.MaxTables = -1 },
		func(o *Options) { o.MaxStatements = -1 },
	} {
		options := testOptions()
		mutate(&options)
		if plan, err := Prepare(context.Background(), strings.NewReader(header+simpleTable), options); plan != nil || !errors.Is(err, ErrOptions) {
			t.Fatal("invalid preflight options were accepted")
		}
	}
}

func TestMariaDBSandboxDirectiveIsOnlyRecognizedExactly(t *testing.T) {
	options := testOptions()
	options.Driver = "mariadb"
	input := "/*M!999999\\- enable sandbox mode */\n" + header + simpleTable
	if _, err := Prepare(context.Background(), strings.NewReader(input), options); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), strings.NewReader(strings.Replace(input, "M!999999", "!999999", 1)), options); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), strings.NewReader(strings.Replace(input, "enable sandbox", "enable the sandbox", 1)), options); err != nil {
		t.Fatal(err)
	}
	for _, driver := range []string{"mysql", "mariadb"} {
		options.Driver = driver
		bad := strings.Replace(input, "enable sandbox mode", "disable sandbox mode", 1)
		if plan, err := Prepare(context.Background(), strings.NewReader(bad), options); plan != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatal("unknown client directive accepted")
		}
	}
}

func TestBoundsCancellationAndSafeOutputErrors(t *testing.T) {
	input := header + simpleTable
	for _, options := range []Options{
		{Driver: "mysql", Source: "original", Destination: testOptions().Destination, MaxInputBytes: 10},
		{Driver: "mysql", Source: "original", Destination: testOptions().Destination, MaxStatementBytes: 10},
		{Driver: "mysql", Source: "original", Destination: testOptions().Destination, MaxStatements: 1},
	} {
		if plan, err := Prepare(context.Background(), strings.NewReader(input), options); plan != nil || !errors.Is(err, ErrLimit) {
			t.Fatalf("resource bound was not enforced: %v", err)
		}
	}
	options := testOptions()
	options.MaxTables = 1
	if plan, err := Prepare(context.Background(), strings.NewReader(input+"CREATE TABLE second (id INT) ENGINE=InnoDB;"), options); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("table metadata bound was not enforced")
	}
	deep := header + "CREATE TABLE nested (id INT DEFAULT " + strings.Repeat("(", 129) + "1" + strings.Repeat(")", 129) + ") ENGINE=InnoDB;"
	if plan, err := Prepare(context.Background(), strings.NewReader(deep), testOptions()); plan != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("SQL nesting bound was not enforced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if plan, err := Prepare(ctx, strings.NewReader(input), testOptions()); plan != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled preflight continued")
	}
	plan, err := Prepare(context.Background(), strings.NewReader(input), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%+v / %#v", plan, plan); strings.Contains(got, "create") || strings.Contains(got, "players") {
		t.Fatal("formatting a plan exposed private SQL")
	}
	called := false
	if err := plan.ForEach(ctx, func(string) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled plan consumption continued")
	}
	private := errors.New("private SQL, key path, and credentials")
	if err := plan.ForEach(context.Background(), func(string) error { return private }); !errors.Is(err, ErrOutput) || !errors.Is(err, private) || strings.Contains(err.Error(), "credentials") {
		t.Fatal("output failure leaked private detail or lost classification")
	}
	if plan, err := Prepare(context.Background(), &failedReader{private}, testOptions()); plan != nil || !errors.Is(err, ErrInput) || strings.Contains(err.Error(), "credentials") {
		t.Fatal("input failure leaked private detail or lost classification")
	}
}

type failedReader struct{ err error }

func (r *failedReader) Read([]byte) (int, error) { return 0, r.err }

func FuzzPrepare(f *testing.F) {
	for _, seed := range []string{header + simpleTable, header + simpleTable + viewSession + simpleView, "/*!999999 SELECT 1 */;", "/*M!999999\\- enable sandbox mode */", `SELECT 'quoted\'';`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 4096 {
			return
		}
		options := testOptions()
		options.MaxInputBytes, options.MaxStatementBytes, options.MaxStatements, options.MaxTables = 4096, 4096, 128, 32
		plan, err := Prepare(context.Background(), strings.NewReader(input), options)
		if err != nil && plan != nil {
			t.Fatal("failed preflight returned a partial plan")
		}
		if plan != nil {
			if err := plan.ForEach(context.Background(), func(sql string) error { _, err := io.Copy(io.Discard, strings.NewReader(sql)); return err }); err != nil {
				t.Fatal(err)
			}
		}
	})
}
