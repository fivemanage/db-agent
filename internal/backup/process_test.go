package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/dump"
	"github.com/fivemanage/db-agent/internal/transfer"
)

func TestMain(m *testing.M) {
	if os.Getenv("DB_AGENT_BACKUP_PROCESS_HELPER") == "1" {
		data := make([]byte, 64<<10)
		for {
			// Repeated blocks compress too well to reliably reach the spool limit.
			if _, err := rand.Read(data); err != nil {
				os.Exit(2)
			}
			if _, err := os.Stdout.Write(data); err != nil {
				os.Exit(2)
			}
		}
	}
	os.Exit(m.Run())
}

func TestRealDumpProcessIsJoinedOnDownstreamLimitAndTimeout(t *testing.T) {
	for _, failure := range []string{"spool", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("DB_AGENT_BACKUP_PROCESS_HELPER", "1")
			t.Setenv("GORACE", "atexit_sleep_ms=0")
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			credentials := t.TempDir()
			options := transfer.Options{MaxBytes: 128 << 10}
			timeout := 5 * time.Second
			if failure == "timeout" {
				options.MaxBytes = 1 << 30
				timeout = 100 * time.Millisecond
			}
			r := testRunner(t, t.TempDir(), &testPlane{}, nil, options)
			settings := config.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 3306, User: "backup", Password: "local-private-password", Databases: []string{"game"}}
			tool := database.Tool{Name: "mysqldump", Path: binary, Version: "8.4.0"}
			prepare := plan(t, artifact.Zstd, func(ctx context.Context, dst io.Writer) error {
				return dump.Run(ctx, dst, settings, tool, dump.Options{Timeout: timeout, TempDir: credentials, ThrottleIO: failure == "timeout"})
			})
			err = r.Run(context.Background(), prepare)
			if err == nil || failure == "spool" && !errors.Is(err, transfer.ErrLimit) || failure == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unexpected process failure: %v", err)
			}
			entries, err := os.ReadDir(credentials)
			if err != nil || len(entries) != 0 {
				t.Fatalf("dump returned before credential cleanup: %v", err)
			}
			assertFiles(t, r.dir, false, false)
		})
	}
}
