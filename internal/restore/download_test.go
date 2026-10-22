package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
)

type lookupFunc func(context.Context, string) (controlplane.DownloadBackupResponse, error)

func (f lookupFunc) DownloadBackup(ctx context.Context, id string) (controlplane.DownloadBackupResponse, error) {
	return f(ctx, id)
}

func remoteRestorer(t *testing.T, server *httptest.Server, metadata artifact.Metadata, lookups *atomic.Int32) *Restorer {
	t.Helper()
	lookup := lookupFunc(func(_ context.Context, id string) (controlplane.DownloadBackupResponse, error) {
		if id != "backup_1" {
			t.Error("backup identity changed during retry")
		}
		lookups.Add(1)
		return controlplane.DownloadBackupResponse{DownloadURL: server.URL + "/backup?signature=private-signature", ExpiresAt: time.Now().Add(time.Hour), SizeBytes: metadata.SizeBytes, Checksum: metadata.Checksum}, nil
	})
	r, err := New(lookup, Options{Transport: server.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	r.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return r
}

func TestRemoteRestoreRenewsExpiredURLsAndInterruptedDownloads(t *testing.T) {
	for _, failure := range []string{"expired URL", "interrupted body", "temporary error"} {
		t.Run(failure, func(t *testing.T) {
			encoded, sql, metadata := encryptedSQL(t, artifact.Gzip)
			var calls, lookups atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Accept-Encoding") != "identity" || request.Method != http.MethodGet {
					t.Error("storage request leaked credentials or changed content encoding")
				}
				if calls.Add(1) == 1 {
					switch failure {
					case "expired URL":
						w.WriteHeader(http.StatusForbidden)
						io.WriteString(w, "private provider error")
						return
					case "interrupted body":
						w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
						w.Write(encoded[:len(encoded)/2])
						return
					case "temporary error":
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				w.Write(encoded)
			}))
			defer server.Close()
			r := remoteRestorer(t, server, metadata, &lookups)
			output := filepath.Join(t.TempDir(), "restore.sql")
			r.wait = func(ctx context.Context, _ time.Duration) error {
				noOutput(t, output)
				return ctx.Err()
			}

			result, err := r.Restore(context.Background(), Request{BackupID: "backup_1", Output: output, Key: recoveryKey(t)})

			if err != nil || result.Artifact != metadata || lookups.Load() != 2 || calls.Load() != 2 {
				t.Fatalf("retry did not renew and restore: %+v %v (%d lookups, %d downloads)", result, err, lookups.Load(), calls.Load())
			}
			contents, err := os.ReadFile(output)
			if err != nil || string(contents) != string(sql) {
				t.Fatal("retried download produced incorrect SQL")
			}
			noStaging(t, output)
		})
	}
}

func TestRemoteFailuresDoNotPublishOrExposeSignedURLs(t *testing.T) {
	for _, failure := range []string{"redirect", "not found", "checksum", "size", "encoding", "tampered", "unavailable"} {
		t.Run(failure, func(t *testing.T) {
			encoded, _, metadata := encryptedSQL(t, artifact.Zstd)
			var calls, lookups atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if request.URL.Path != "/backup" {
					t.Error("storage redirect was followed")
				}
				switch failure {
				case "redirect":
					w.Header().Set("Location", "/private-signature")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				case "not found":
					w.WriteHeader(http.StatusNotFound)
					io.WriteString(w, "private provider error")
					return
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				case "unavailable":
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Write(encoded)
			}))
			defer server.Close()
			if failure == "checksum" {
				metadata.Checksum = "sha256:" + strings.Repeat("f", 64)
			}
			if failure == "size" {
				metadata.SizeBytes++
			}
			if failure == "tampered" {
				encoded[len(encoded)-1] ^= 1
			}
			r := remoteRestorer(t, server, metadata, &lookups)
			output := filepath.Join(t.TempDir(), "restore.sql")

			_, err := r.Restore(context.Background(), Request{BackupID: "backup_1", Output: output, Key: recoveryKey(t)})

			if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "signature") {
				t.Fatalf("download failure was accepted or exposed: %v", err)
			}
			wantCalls := int32(1)
			if failure == "unavailable" {
				wantCalls = 3
			}
			if calls.Load() != wantCalls || lookups.Load() != wantCalls {
				t.Fatalf("incorrect retry count: %d downloads, %d lookups", calls.Load(), lookups.Load())
			}
			noOutput(t, output)
		})
	}
}

func TestURLRenewalCannotChangeTrustedArtifactIdentity(t *testing.T) {
	_, _, metadata := encryptedSQL(t, artifact.Gzip)
	var calls, lookups atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	r := remoteRestorer(t, server, metadata, &lookups)
	original := r.lookup
	r.lookup = lookupFunc(func(ctx context.Context, id string) (controlplane.DownloadBackupResponse, error) {
		target, err := original.DownloadBackup(ctx, id)
		if lookups.Load() == 2 {
			target.Checksum = "sha256:" + strings.Repeat("f", 64)
		}
		return target, err
	})
	output := filepath.Join(t.TempDir(), "restore.sql")

	_, err := r.Restore(context.Background(), Request{BackupID: "backup_1", Output: output, Key: recoveryKey(t)})

	if !errors.Is(err, ErrIntegrity) || calls.Load() != 1 || lookups.Load() != 2 {
		t.Fatalf("URL renewal switched artifact: %v", err)
	}
	noOutput(t, output)
}

func TestDownloadCancellationClosesBlockedBodyAndDiscardsOutput(t *testing.T) {
	encoded, _, metadata := encryptedSQL(t, artifact.Gzip)
	started := make(chan struct{})
	stopped := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer close(stopped)
		w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
		w.Write(encoded[:44])
		w.(http.Flusher).Flush()
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	var lookups atomic.Int32
	r := remoteRestorer(t, server, metadata, &lookups)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := filepath.Join(t.TempDir(), "restore.sql")
	done := make(chan error, 1)
	go func() {
		_, err := r.Restore(ctx, Request{BackupID: "backup_1", Output: output, Key: recoveryKey(t)})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked download lost cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop the download")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled download did not close its connection")
	}
	noOutput(t, output)
}

func TestRejectedCredentialsNeverReachStorage(t *testing.T) {
	lookup := lookupFunc(func(context.Context, string) (controlplane.DownloadBackupResponse, error) {
		return controlplane.DownloadBackupResponse{}, controlplane.ErrCredentialRejected
	})
	r, err := New(lookup, Options{})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "restore.sql")

	_, err = r.Restore(context.Background(), Request{BackupID: "backup_1", Output: output, Key: recoveryKey(t)})

	if !errors.Is(err, controlplane.ErrCredentialRejected) {
		t.Fatalf("rejected credential lost: %v", err)
	}
	noOutput(t, output)
}

func TestRetryAfterCannotOutliveRestoreDeadline(t *testing.T) {
	_, _, metadata := encryptedSQL(t, artifact.Gzip)
	var calls, lookups atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	r := remoteRestorer(t, server, metadata, &lookups)
	r.options.Timeout = time.Second
	r.wait = wait
	output := filepath.Join(t.TempDir(), "restore.sql")

	_, err := r.Restore(context.Background(), Request{BackupID: "backup_1", Output: output, Key: recoveryKey(t)})

	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("Retry-After was ignored or escaped the total deadline: %v (%d calls)", err, calls.Load())
	}
	noOutput(t, output)
}

func TestUnsafeDownloadLookupNeverReachesStorage(t *testing.T) {
	_, _, metadata := encryptedSQL(t, artifact.Gzip)
	for _, target := range []controlplane.DownloadBackupResponse{
		{DownloadURL: "http://storage.invalid/private-signature", ExpiresAt: time.Now().Add(time.Hour), SizeBytes: metadata.SizeBytes, Checksum: metadata.Checksum},
		{DownloadURL: "https://user:password@storage.invalid/private-signature", ExpiresAt: time.Now().Add(time.Hour), SizeBytes: metadata.SizeBytes, Checksum: metadata.Checksum},
		{DownloadURL: "https://storage.invalid/private-signature", ExpiresAt: time.Now().Add(-time.Hour), SizeBytes: metadata.SizeBytes, Checksum: metadata.Checksum},
		{DownloadURL: "https://storage.invalid/private-signature", ExpiresAt: time.Now().Add(time.Hour), SizeBytes: metadata.SizeBytes},
	} {
		lookup := lookupFunc(func(context.Context, string) (controlplane.DownloadBackupResponse, error) { return target, nil })
		r, err := New(lookup, Options{})
		if err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(t.TempDir(), "restore.sql")

		_, err = r.Restore(context.Background(), Request{BackupID: "backup_1", Output: output, Key: recoveryKey(t)})

		if !errors.Is(err, ErrDownload) || strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "password") {
			t.Fatalf("unsafe lookup accepted or exposed: %v", err)
		}
		noOutput(t, output)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cancelOnClose struct {
	io.Reader
	cancel context.CancelFunc
}

func (b cancelOnClose) Close() error {
	b.cancel()
	return nil
}

func TestCancellationAfterPublicationDoesNotReportFailedRestore(t *testing.T) {
	encoded, _, metadata := encryptedSQL(t, artifact.Zstd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lookup := lookupFunc(func(context.Context, string) (controlplane.DownloadBackupResponse, error) {
		return controlplane.DownloadBackupResponse{DownloadURL: "https://storage.example/backup", ExpiresAt: time.Now().Add(time.Hour), SizeBytes: metadata.SizeBytes, Checksum: metadata.Checksum}, nil
	})
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: metadata.SizeBytes, Body: cancelOnClose{Reader: bytes.NewReader(encoded), cancel: cancel}}, nil
	})
	r, err := New(lookup, Options{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "restore.sql")

	result, err := r.Restore(ctx, Request{BackupID: "backup_1", Output: output, Key: recoveryKey(t)})

	if err != nil || result.Artifact != metadata || ctx.Err() == nil {
		t.Fatalf("post-publication cancellation changed the outcome: %v", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal("completed restore output was lost")
	}
	noStaging(t, output)
}
