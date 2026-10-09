package dump

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("DB_AGENT_DUMP_HELPER") == "1" {
		helperProcess()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperProcess() {
	mode := os.Getenv("DB_AGENT_DUMP_MODE")
	switch mode {
	case "describe":
		options := strings.TrimPrefix(os.Args[1], "--defaults-file=")
		contents, err := os.ReadFile(options)
		if err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Args    []string
			Env     []string
			Options string
		}{os.Args[1:], os.Environ(), string(contents)})
	case "stream":
		block := bytes.Repeat([]byte("SQL;\n"), 6553)
		for range 256 {
			if _, err := os.Stdout.Write(block); err != nil {
				os.Exit(2)
			}
		}
	case "error":
		_, _ = io.WriteString(os.Stderr, os.Getenv("DB_AGENT_DUMP_STDERR"))
		os.Exit(2)
	case "stderr flood":
		_, _ = io.Copy(os.Stderr, io.LimitReader(repeatingReader{}, 4<<20))
		os.Exit(2)
	case "spawn", "spawn-exit":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "DB_AGENT_DUMP_MODE=sleep", "DB_AGENT_DUMP_READY=")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		ready := fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)
		_ = os.WriteFile(os.Getenv("DB_AGENT_DUMP_READY"), []byte(ready), 0600)
		if mode == "spawn-exit" {
			return
		}
		for {
			time.Sleep(time.Second)
		}
	case "sleep":
		if path := os.Getenv("DB_AGENT_DUMP_READY"); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
		}
		for {
			time.Sleep(time.Second)
		}
	default:
		os.Exit(4)
	}
}

type repeatingReader struct{}

func (repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func helperMode(t testing.TB, mode string) {
	t.Helper()
	t.Setenv("DB_AGENT_DUMP_HELPER", "1")
	t.Setenv("DB_AGENT_DUMP_MODE", mode)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
}

func TestRunStreamsAndCleansCredentials(t *testing.T) {
	helperMode(t, "describe")
	t.Setenv("MYSQL_PWD", "ambient-secret")
	parent := t.TempDir()
	var output bytes.Buffer
	err := Run(context.Background(), &output, testSettings(), testTool(t), Options{TempDir: parent})
	if err != nil {
		t.Fatal(err)
	}
	var description struct {
		Args    []string
		Env     []string
		Options string
	}
	if err := json.Unmarshal(output.Bytes(), &description); err != nil {
		t.Fatal("child did not receive its private options")
	}
	if !strings.Contains(description.Options, `password="private-test-password"`) || !strings.Contains(description.Options, `host="127.0.0.1"`) {
		t.Fatal("child did not receive the correct local connection")
	}
	if strings.Contains(strings.Join(description.Args, " ")+strings.Join(description.Env, " "), "private-test-password") || strings.Contains(strings.Join(description.Env, " "), "ambient-secret") {
		t.Fatal("credentials leaked into child arguments/environment")
	}
	assertEmptyDir(t, parent)
}

func TestRunLargeOutputWithoutArtifactBuffers(t *testing.T) {
	helperMode(t, "stream")
	parent := t.TempDir()
	output := &countWriter{}
	if err := Run(context.Background(), output, testSettings(), testTool(t), Options{TempDir: parent}); err != nil {
		t.Fatal(err)
	}
	if output.n != 6553*5*256 || output.largest > 32<<10 {
		t.Fatalf("bytes=%d largest write=%d", output.n, output.largest)
	}
	assertEmptyDir(t, parent)
}

type countWriter struct {
	n       int
	largest int
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	if len(p) > w.largest {
		w.largest = len(p)
	}
	return len(p), nil
}

func TestRunRedactsAndClassifiesStderr(t *testing.T) {
	for _, test := range []struct {
		text string
		want error
	}{
		{"mysqldump: Got error: 1045: private-test-password", ErrAuthentication},
		{"mariadb-dump: Got error: 1142: private-test-password", ErrPrivileges},
		{"mysqldump: Got error: 1049: private-test-password", ErrDatabase},
		{"mysqldump: Got error: 1021: private-test-password", ErrDiskFull},
		{"mysqldump: Got error: 2013: private-test-password", ErrConnection},
		{"arbitrary SQL and private-test-password", ErrFailed},
	} {
		t.Run(test.want.Error(), func(t *testing.T) {
			helperMode(t, "error")
			t.Setenv("DB_AGENT_DUMP_STDERR", test.text)
			parent := t.TempDir()
			err := Run(context.Background(), io.Discard, testSettings(), testTool(t), Options{TempDir: parent})
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "private-test-password") {
				t.Fatalf("unsafe or incorrect error: %v", err)
			}
			assertEmptyDir(t, parent)
		})
	}
	helperMode(t, "stderr flood")
	if err := Run(context.Background(), io.Discard, testSettings(), testTool(t), Options{TempDir: t.TempDir()}); !errors.Is(err, ErrFailed) {
		t.Fatalf("stderr flood: %v", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestOutputFailureKillsAndCleansUp(t *testing.T) {
	for _, test := range []struct {
		name string
		dst  io.Writer
		want error
	}{
		{"disk full", failingWriter{syscall.ENOSPC}, ErrDiskFull},
		{"permissions", failingWriter{os.ErrPermission}, ErrPermission},
		{"redacted downstream error", failingWriter{errors.New("private-test-password")}, ErrOutput},
		{"short write", shortWriter{}, ErrOutput},
	} {
		t.Run(test.name, func(t *testing.T) {
			helperMode(t, "stream")
			parent := t.TempDir()
			err := Run(context.Background(), test.dst, testSettings(), testTool(t), Options{TempDir: parent, Timeout: 5 * time.Second})
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "private-test-password") {
				t.Fatalf("unsafe or incorrect error: %v", err)
			}
			assertEmptyDir(t, parent)
		})
	}
}

func TestCancellationAndTimeoutCleanProcessTree(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			helperMode(t, "spawn")
			ready := filepath.Join(t.TempDir(), "ready")
			t.Setenv("DB_AGENT_DUMP_READY", ready)
			parent := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			options := Options{TempDir: parent, Timeout: 5 * time.Second}
			if timeout {
				options.Timeout = 500 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, io.Discard, testSettings(), testTool(t), options) }()
			pids := waitReady(t, ready)
			if !timeout {
				cancel()
			}
			select {
			case err := <-done:
				want := context.Canceled
				if timeout {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) {
					t.Fatalf("cancellation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation failed to join the process tree")
			}
			for _, pid := range pids {
				assertProcessStopped(t, pid)
			}
			assertEmptyDir(t, parent)
		})
	}
}

func TestNaturalExitCleansLingeringChildren(t *testing.T) {
	helperMode(t, "spawn-exit")
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("DB_AGENT_DUMP_READY", ready)
	parent := t.TempDir()
	if err := Run(context.Background(), io.Discard, testSettings(), testTool(t), Options{TempDir: parent, Timeout: 3 * time.Second}); err != nil {
		t.Fatal(err)
	}
	for _, pid := range waitReady(t, ready) {
		assertProcessStopped(t, pid)
	}
	assertEmptyDir(t, parent)
}

func TestCanceledBeforeStartLeavesNoFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	parent := t.TempDir()
	err := Run(ctx, io.Discard, testSettings(), testTool(t), Options{TempDir: parent})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertEmptyDir(t, parent)
}

func TestStartAndCredentialFailures(t *testing.T) {
	parent := t.TempDir()
	tool := testTool(t)
	tool.Path = filepath.Join(parent, "missing-dump-client")
	err := Run(context.Background(), io.Discard, testSettings(), tool, Options{TempDir: parent})
	if !errors.Is(err, ErrStart) {
		t.Fatal(err)
	}
	assertEmptyDir(t, parent)
	if err := Run(context.Background(), io.Discard, testSettings(), testTool(t), Options{TempDir: filepath.Join(parent, "missing", "nested")}); !errors.Is(err, ErrCredentials) {
		t.Fatal(err)
	}
}

func waitReady(t testing.TB, path string) []int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			var pids []int
			for _, field := range strings.Fields(string(data)) {
				pid, err := strconv.Atoi(field)
				if err != nil {
					t.Fatal(err)
				}
				pids = append(pids, pid)
			}
			return pids
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not start")
	return nil
}

func assertProcessStopped(t testing.TB, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dump child %d survived", pid)
}

func TestThrottleIsBoundedAndCancelable(t *testing.T) {
	ctx := context.Background()
	started := time.Now()
	if err := copyOutput(ctx, io.Discard, bytes.NewReader(make([]byte, 8<<10)), 32<<10); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 250*time.Millisecond {
		t.Fatal("dump output exceeded the configured rate")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	started = time.Now()
	if err := copyOutput(ctx, io.Discard, bytes.NewReader(make([]byte, 32<<10)), 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("throttle wait did not honor cancellation")
	}
}
