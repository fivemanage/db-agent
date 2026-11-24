package restoreplan

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGeneratedColumnsAndJSONDefaultsArePlannedSafely(t *testing.T) {
	t.Run("overextended mariadb schema and positional insert", func(t *testing.T) {
		options := testOptions()
		options.Driver = "mariadb"
		input := header +
			"CREATE TABLE `characters` (" +
			"`charId` int(10) unsigned NOT NULL AUTO_INCREMENT," +
			"`firstName` varchar(50) NOT NULL," +
			"`lastName` varchar(50) NOT NULL," +
			"`fullName` varchar(101) GENERATED ALWAYS AS (concat(`firstName`,' ',`lastName`)) STORED," +
			"`statuses` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT json_object() CHECK (json_valid(`statuses`))," +
			"`items` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT json_array() CHECK (json_valid(`items`))," +
			"PRIMARY KEY (`charId`)," +
			"FULLTEXT KEY `characters_fullName_index` (`fullName`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;" +
			"INSERT INTO `characters` VALUES (4,'Peter','Linden','Peter Linden','{\"hunger\":100}','[]');"

		memory, err := Prepare(context.Background(), strings.NewReader(input), options)
		if err != nil {
			t.Fatal(err)
		}
		file, err := PrepareFile(context.Background(), privateSQLFile(t, input), options)
		if err != nil {
			t.Fatal(err)
		}

		output := statements(t, file)
		if !reflect.DeepEqual(output, statements(t, memory)) {
			t.Fatalf("file-backed plan differed from in-memory plan:\nfile: %v\nmem:  %v", output, statements(t, memory))
		}
		if !file.HasJSONChecks() || !reflect.DeepEqual(file.Tables(), []Table{{Name: "characters", Rows: 1}}) {
			t.Fatalf("unexpected metadata: jsonChecks=%v tables=%+v", file.HasJSONChecks(), file.Tables())
		}

		joined := strings.Join(output, ";\n")
		if !strings.Contains(joined, "default (json_object())") || !strings.Contains(joined, "default (json_array())") {
			t.Fatalf("empty JSON defaults were not preserved:\n%s", joined)
		}
		if !strings.Contains(joined, "as (concat(firstName, ' ', lastName)) stored") {
			t.Fatalf("generated column definition was not preserved:\n%s", joined)
		}
		if strings.Contains(joined, "'Peter Linden'") || !strings.Contains(joined, "values (4, 'Peter', 'Linden', default,") {
			t.Fatalf("generated column positional value was not rewritten to default:\n%s", joined)
		}
	})

	t.Run("esx generated column and mysql explicit column list", func(t *testing.T) {
		options := testOptions()
		options.Driver = "mysql"
		input := header +
			"CREATE TABLE `owned_vehicles` (" +
			"`id` int NOT NULL AUTO_INCREMENT," +
			"`owner` varchar(60) DEFAULT NULL," +
			"`owner_key` varchar(60) GENERATED ALWAYS AS (coalesce(`owner`,'')) STORED," +
			"`meta` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT (json_object())," +
			"PRIMARY KEY (`id`)," +
			"UNIQUE KEY `idx_owner_key` (`owner_key`)," +
			"CHECK (json_valid(`meta`))" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;" +
			"INSERT INTO `owned_vehicles` (`id`, `owner`, `meta`) VALUES (1, 'char1:abc', '{}');"

		memory, err := Prepare(context.Background(), strings.NewReader(input), options)
		if err != nil {
			t.Fatal(err)
		}
		file, err := PrepareFile(context.Background(), privateSQLFile(t, input), options)
		if err != nil {
			t.Fatal(err)
		}

		output := statements(t, file)
		if !reflect.DeepEqual(output, statements(t, memory)) {
			t.Fatal("file-backed plan differed from in-memory plan")
		}
		joined := strings.Join(output, ";\n")
		if !strings.Contains(joined, "as (coalesce(owner, '')) stored") {
			t.Fatalf("generated column was not preserved:\n%s", joined)
		}
		if !strings.Contains(joined, "insert into fm_restore_") || !strings.Contains(joined, "(id, owner, meta) values (1, 'char1:abc', '{}')") {
			t.Fatalf("explicit non-generated column insert was not preserved:\n%s", joined)
		}
	})
}

func TestUnsafeGeneratedColumnsAndJSONDefaultsRejectTheWholePlan(t *testing.T) {
	cases := map[string]string{
		"udf":                  "CREATE TABLE t (a varchar(20), g varchar(20) GENERATED ALWAYS AS (evil_udf(a)) STORED) ENGINE=InnoDB;",
		"nondeterministic_now": "CREATE TABLE t (a int, g datetime GENERATED ALWAYS AS (now()) STORED) ENGINE=InnoDB;",
		"subquery":             "CREATE TABLE t (a int, g int GENERATED ALWAYS AS ((SELECT id FROM other.t LIMIT 1)) STORED) ENGINE=InnoDB;",
		"variable":             "CREATE TABLE t (a int, g int GENERATED ALWAYS AS (@private) STORED) ENGINE=InnoDB;",
		"qualified_column":     "CREATE TABLE t (a int, g int GENERATED ALWAYS AS (t.a) STORED) ENGINE=InnoDB;",
		"other_schema_column":  "CREATE TABLE t (a int, g int GENERATED ALWAYS AS (other.t.a) STORED) ENGINE=InnoDB;",
		"missing_column":       "CREATE TABLE t (a int, g int GENERATED ALWAYS AS (missing) STORED) ENGINE=InnoDB;",
		"chained_generated":    "CREATE TABLE t (a int, g1 int GENERATED ALWAYS AS (a + 1) STORED, g2 int GENERATED ALWAYS AS (g1 + 1) STORED) ENGINE=InnoDB;",
		"self_reference":       "CREATE TABLE t (g int GENERATED ALWAYS AS (g + 1) STORED) ENGINE=InnoDB;",
		"non_empty_json_obj":   "CREATE TABLE t (d longtext NOT NULL DEFAULT (json_object('k', 'private'))) ENGINE=InnoDB;",
		"non_empty_json_arr":   "CREATE TABLE t (d longtext NOT NULL DEFAULT (json_array('private'))) ENGINE=InnoDB;",
		"on_update_json_obj":   "CREATE TABLE t (d longtext NOT NULL ON UPDATE (json_object())) ENGINE=InnoDB;",
		"unsafe_inline_check":  "CREATE TABLE t (d longtext NOT NULL DEFAULT json_object() CHECK (GET_LOCK('private', 0))) ENGINE=InnoDB;",
		"default_with_gen_col": "CREATE TABLE t (a int, g int GENERATED ALWAYS AS (a + 1) STORED DEFAULT 1) ENGINE=InnoDB;",
	}

	for name, ddl := range cases {
		t.Run(name, func(t *testing.T) {
			input := header + simpleTable + ddl
			for _, fileBacked := range []bool{false, true} {
				var plan *Plan
				var err error
				if fileBacked {
					plan, err = PrepareFile(context.Background(), privateSQLFile(t, input), testOptions())
				} else {
					plan, err = Prepare(context.Background(), strings.NewReader(input), testOptions())
				}
				if plan != nil || !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "private") {
					t.Fatalf("unsafe DDL (%s) produced plan or leaked SQL: plan=%v err=%v", name, plan, err)
				}
			}
		})
	}
}
