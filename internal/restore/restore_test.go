package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/securefile"
)

func recoveryKey(t *testing.T) key.Key {
	t.Helper()
	k, err := key.Parse("dbak1_" + strings.Repeat("A", 42) + "E")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func encryptedSQL(t *testing.T, compression artifact.Compression) ([]byte, []byte, artifact.Metadata) {
	t.Helper()
	sql := []byte("CREATE DATABASE `original`;\nUSE `original`;\n" + strings.Repeat("INSERT INTO t VALUES ('private SQL');\n", 5000))
	var encoded bytes.Buffer
	metadata, err := artifact.Encrypt(context.Background(), &encoded, bytes.NewReader(sql), recoveryKey(t), compression)
	if err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes(), sql, metadata
}

func localFile(t *testing.T, data []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, "backup.dbak")
	if err := os.WriteFile(input, data, 0644); err != nil {
		t.Fatal(err)
	}
	return input, filepath.Join(dir, "restore.sql")
}

func restorer(t *testing.T, options Options) *Restorer {
	t.Helper()
	r, err := New(nil, options)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func noOutput(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete output was published: %v", err)
	}
	noStaging(t, path)
}

func noStaging(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".db-agent-restore-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("private plaintext staging was not removed: %v %v", matches, err)
	}
}

func TestLocalRestorePublishesCompletePrivateSQL(t *testing.T) {
	for _, compression := range []artifact.Compression{artifact.Zstd, artifact.Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			encoded, sql, metadata := encryptedSQL(t, compression)
			input, output := localFile(t, encoded)

			result, err := restorer(t, Options{}).Restore(context.Background(), Request{File: input, Output: output, Key: recoveryKey(t)})

			if err != nil || result.Artifact != metadata || result.OutputBytes != int64(len(sql)) {
				t.Fatalf("restore failed: %+v %v", result, err)
			}
			decoded, err := securefile.Read(output, int64(len(sql)))
			if err != nil || !bytes.Equal(decoded, sql) {
				t.Fatalf("output was not complete and private: %v", err)
			}
			original, err := os.ReadFile(input)
			if err != nil || !bytes.Equal(original, encoded) {
				t.Fatal("restore changed its encrypted input")
			}
			noStaging(t, output)
		})
	}
}

func TestInvalidArtifactsNeverPublishPlaintext(t *testing.T) {
	encoded, _, _ := encryptedSQL(t, artifact.Gzip)
	modified := bytes.Clone(encoded)
	modified[len(modified)-1] ^= 1
	wrongKey, err := key.Parse("dbak1_" + strings.Repeat("B", 42) + "E")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		key  key.Key
	}{
		{"wrong key", encoded, wrongKey},
		{"tampered terminal", modified, recoveryKey(t)},
		{"truncated", encoded[:len(encoded)-1], recoveryKey(t)},
		{"trailing bytes", append(bytes.Clone(encoded), 1), recoveryKey(t)},
		{"plaintext", []byte("private SQL password"), recoveryKey(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, output := localFile(t, tc.data)

			_, err := restorer(t, Options{}).Restore(context.Background(), Request{File: input, Output: output, Key: tc.key})

			if !errors.Is(err, ErrIntegrity) || strings.Contains(err.Error(), "private SQL") {
				t.Fatalf("invalid artifact accepted or exposed: %v", err)
			}
			noOutput(t, output)
		})
	}
}

func TestExistingDestinationIsNeverOverwritten(t *testing.T) {
	encoded, _, _ := encryptedSQL(t, artifact.Zstd)
	input, output := localFile(t, encoded)
	if err := os.WriteFile(output, []byte("keep existing output"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := restorer(t, Options{}).Restore(context.Background(), Request{File: input, Output: output, Key: recoveryKey(t)})

	if !errors.Is(err, ErrExists) {
		t.Fatalf("existing output was accepted: %v", err)
	}
	contents, err := os.ReadFile(output)
	if err != nil || string(contents) != "keep existing output" {
		t.Fatal("existing output was modified")
	}
	noStaging(t, output)
}

func TestNewerArtifactFormatRequestsAnAgentUpdateWithoutOutput(t *testing.T) {
	encoded, _, _ := encryptedSQL(t, artifact.Zstd)
	encoded[8] = 2
	input, output := localFile(t, encoded)

	_, err := restorer(t, Options{}).Restore(context.Background(), Request{File: input, Output: output, Key: recoveryKey(t)})

	if !errors.Is(err, ErrFormat) || !strings.Contains(err.Error(), "update db-agent") {
		t.Fatalf("unsupported format has no upgrade guidance: %v", err)
	}
	noOutput(t, output)
}

func TestDestinationCreatedDuringRestoreIsNotOverwritten(t *testing.T) {
	encoded, _, _ := encryptedSQL(t, artifact.Zstd)
	input, output := localFile(t, encoded)
	r := restorer(t, Options{})
	calls := 0
	r.freeSpace = func(string) (uint64, error) {
		calls++
		if calls == 2 {
			if err := os.WriteFile(output, []byte("concurrent writer"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return 1 << 40, nil
	}

	_, err := r.Restore(context.Background(), Request{File: input, Output: output, Key: recoveryKey(t)})

	if !errors.Is(err, ErrExists) {
		t.Fatalf("concurrent output was accepted: %v", err)
	}
	contents, err := os.ReadFile(output)
	if err != nil || string(contents) != "concurrent writer" {
		t.Fatal("concurrent output was modified")
	}
	noStaging(t, output)
}

func TestRestoreLimitsAndDiskPressureDiscardOutput(t *testing.T) {
	encoded, _, _ := encryptedSQL(t, artifact.Zstd)
	for _, scenario := range []string{"input limit", "output limit", "disk before", "disk during", "stat failure"} {
		t.Run(scenario, func(t *testing.T) {
			input, output := localFile(t, encoded)
			options := Options{}
			if scenario == "input limit" {
				options.MaxInputBytes = int64(len(encoded) - 1)
			}
			if scenario == "output limit" {
				options.MaxOutputBytes = 32769
			}
			r := restorer(t, options)
			calls := 0
			r.freeSpace = func(string) (uint64, error) {
				calls++
				if scenario == "stat failure" {
					return 0, errors.New("private disk path")
				}
				if scenario == "disk before" || scenario == "disk during" && calls >= 3 {
					return r.options.ReserveBytes, nil
				}
				return 1 << 40, nil
			}

			_, err := r.Restore(context.Background(), Request{File: input, Output: output, Key: recoveryKey(t)})

			if err == nil || strings.Contains(err.Error(), "private disk path") {
				t.Fatalf("restore limit failed or leaked details: %v", err)
			}
			noOutput(t, output)
		})
	}
}

func TestCancellationDuringDecodeDiscardsPrivateOutput(t *testing.T) {
	encoded, _, _ := encryptedSQL(t, artifact.Gzip)
	input, output := localFile(t, encoded)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := restorer(t, Options{})
	calls := 0
	r.freeSpace = func(string) (uint64, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return 1 << 40, nil
	}

	_, err := r.Restore(ctx, Request{File: input, Output: output, Key: recoveryKey(t)})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	noOutput(t, output)
}

func TestOutputWriteFailureIsClassifiedWithoutPrivateDetails(t *testing.T) {
	w := &outputWriter{ctx: context.Background(), file: failingWriter{}, limit: 100, freeSpace: func(string) (uint64, error) { return 1000, nil }}
	_, err := w.Write([]byte("sensitive SQL"))
	if !errors.Is(err, ErrOutput) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("write failure not classified: %v", err)
	}
	w.file = shortWriter{}
	_, err = w.Write([]byte("SQL"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short output write was accepted")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("secret disk details") }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
