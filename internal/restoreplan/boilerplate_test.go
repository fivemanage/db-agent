package restoreplan

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNativeAutocommitBookkeepingIsNeverReplayed(t *testing.T) {
	input := header + simpleTable + `
/*!40101 SET @OLD_AUTOCOMMIT=@@AUTOCOMMIT, AUTOCOMMIT=0 */;
INSERT INTO players VALUES (1,'original.players',NULL);
COMMIT;
/*!40101 SET AUTOCOMMIT=@OLD_AUTOCOMMIT */;
/*!40101 SET @OLD_AUTOCOMMIT=@@AUTOCOMMIT, AUTOCOMMIT=0 */;
INSERT INTO players VALUES (2,'second table data block',NULL);
COMMIT;
/*!40101 SET AUTOCOMMIT=@OLD_AUTOCOMMIT */;
`
	for _, driver := range []string{"mysql", "mariadb"} {
		t.Run(driver, func(t *testing.T) {
			options := testOptions()
			options.Driver = driver
			memory, err := Prepare(context.Background(), strings.NewReader(input), options)
			if err != nil {
				t.Fatal(err)
			}
			file, err := PrepareFile(context.Background(), privateSQLFile(t, input), options)
			if err != nil {
				t.Fatal(err)
			}
			output := statements(t, file)
			if !reflect.DeepEqual(output, statements(t, memory)) || file.Tables()[0].Rows != 2 {
				t.Fatal("native autocommit bookkeeping changed data or plan reconstruction")
			}
			joined := strings.ToLower(strings.Join(output, ";\n"))
			if strings.Contains(joined, "autocommit") || strings.Contains(joined, "@old") || strings.Contains(joined, "commit") {
				t.Fatal("dump autocommit state entered the execution plan")
			}
		})
	}
}

func TestUnsafeAutocommitAndTransactionFormsRejectThePlan(t *testing.T) {
	for _, tail := range []string{
		"SET GLOBAL autocommit=0;",
		"SET @@GLOBAL.autocommit=0;",
		"SET @OLD_AUTOCOMMIT=@@GLOBAL.autocommit;",
		"SET @OLD_AUTOCOMMIT=@@foreign_key_checks;",
		"SET @OTHER=@@autocommit;",
		"SET autocommit=@OTHER;",
		"SET autocommit=GET_LOCK('private',0);",
		"SET @OLD_AUTOCOMMIT=GET_LOCK('private',0);",
		"SET autocommit=2;",
		"SET autocommit=0;", "SET autocommit=1;",
		"SET @OLD_AUTOCOMMIT=@@AUTOCOMMIT, AUTOCOMMIT=0;",
		"SET @OLD_AUTOCOMMIT=@@AUTOCOMMIT, AUTOCOMMIT=0; COMMIT;",
		"SET @OLD_AUTOCOMMIT=@@AUTOCOMMIT, AUTOCOMMIT=0; SET AUTOCOMMIT=@OLD_AUTOCOMMIT;",
		"SET @OLD_AUTOCOMMIT=@@AUTOCOMMIT, AUTOCOMMIT=0; COMMIT; COMMIT; SET AUTOCOMMIT=@OLD_AUTOCOMMIT;",
		"SET @OLD_AUTOCOMMIT=@@AUTOCOMMIT, AUTOCOMMIT=0; SET @OLD_AUTOCOMMIT=@@AUTOCOMMIT, AUTOCOMMIT=0; COMMIT; SET AUTOCOMMIT=@OLD_AUTOCOMMIT;",
		"START TRANSACTION;", "COMMIT;", "ROLLBACK;", "SAVEPOINT private;",
	} {
		t.Run(tail, func(t *testing.T) {
			input := header + simpleTable + "INSERT INTO players VALUES (1,'accepted prefix',NULL);" + tail
			plan, err := PrepareFile(context.Background(), privateSQLFile(t, input), testOptions())
			if plan != nil || !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe transaction/session SQL produced a plan or leaked data: %v", err)
			}
		})
	}
}
