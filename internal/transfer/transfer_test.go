package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
)

type fakePlane struct {
	mu            sync.Mutex
	url           string
	initiations   []controlplane.InitiateBackupRequest
	reports       []controlplane.CompleteBackupRequest
	initiateError error
	completeError error
	onInitiate    func(int) controlplane.InitiateBackupResponse
}

func (p *fakePlane) InitiateBackup(_ context.Context, in controlplane.InitiateBackupRequest) (controlplane.InitiateBackupResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.initiations = append(p.initiations, in)
	if p.initiateError != nil {
		return controlplane.InitiateBackupResponse{}, p.initiateError
	}
	if p.onInitiate != nil {
		return p.onInitiate(len(p.initiations)), nil
	}

	return controlplane.InitiateBackupResponse{
		BackupID:  "backup_1",
		UploadURL: p.url,
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

func (p *fakePlane) CompleteBackup(_ context.Context, id string, in controlplane.CompleteBackupRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if id != "backup_1" {
		return errors.New("unexpected backup ID")
	}

	p.reports = append(p.reports, in)

	return p.completeError
}

func testKey(t *testing.T) key.Key {
	t.Helper()

	k, err := key.Parse("dbak1_" + strings.Repeat("A", 42) + "E")
	if err != nil {
		t.Fatal(err)
	}

	return k
}

func testUploader(t *testing.T, plane ControlPlane, storage *httptest.Server, options Options) *Uploader {
	t.Helper()

	u, err := New(t.TempDir(), plane, options)
	if err != nil {
		t.Fatal(err)
	}
	if storage != nil {
		u.http.Transport = storage.Client().Transport
	}

	return u
}

func assertEmpty(t *testing.T, u *Uploader) {
	t.Helper()

	for _, path := range []string{u.statePath(), u.artifactPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transfer slot wasn't released: %v", err)
		}
	}
}

func assertStable(t *testing.T, requests []controlplane.InitiateBackupRequest) {
	t.Helper()

	if len(requests) == 0 || !identityPattern.MatchString(requests[0].RequestID) {
		t.Fatal("missing stable request identity")
	}

	for _, request := range requests[1:] {
		if !reflect.DeepEqual(request, requests[0]) {
			t.Fatal("retry changed initiation identity or metadata")
		}
	}
}

func TestSuccessfulUploadWithRealControlPlane(t *testing.T) {
	plain := []byte("CREATE DATABASE private_database; INSERT INTO secrets VALUES ('database-password');")
	var uploaded []byte
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "" ||
			r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength <= 0 {
			t.Error("invalid storage request or forwarded credential")
		}

		var err error
		uploaded, err = io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}

		w.WriteHeader(204)
	}))
	defer storage.Close()

	var completion controlplane.CompleteBackupRequest
	var identity string
	credential := "dac_" + strings.Repeat("a", 43)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("missing agent authentication")
		}

		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, plain) || bytes.Contains(body, []byte("database-password")) ||
			bytes.Contains(body, []byte(testKey(t).RecoveryString())) {
			t.Error("control plane received secrets")
		}

		switch r.URL.Path {
		case "/api/v1/backups/initiate":
			identity = r.Header.Get("Idempotency-Key")
			var in controlplane.InitiateBackupRequest
			if err := json.Unmarshal(body, &in); err != nil || in.StartedAt.IsZero() ||
				in.EstimatedSizeBytes == nil || *in.EstimatedSizeBytes != 1000 {
				t.Error("initiation metadata missing")
			}

			w.WriteHeader(201)
			json.NewEncoder(w).Encode(controlplane.InitiateBackupResponse{
				BackupID:  "backup_1",
				UploadURL: storage.URL + "?signature=secret",
				ExpiresAt: time.Now().Add(time.Hour),
			})
		case "/api/v1/backups/backup_1/complete":
			if err := json.Unmarshal(body, &completion); err != nil {
				t.Error(err)
			}

			io.WriteString(w, "{}")
		default:
			t.Error("unexpected API call")
			w.WriteHeader(404)
		}
	}))
	defer api.Close()

	client, err := controlplane.New(api.URL, credential, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	u := testUploader(t, client, storage, Options{})
	estimate := int64(1000)

	if err := u.Run(context.Background(), io.NopCloser(bytes.NewReader(plain)), testKey(t), artifact.Zstd, &estimate); err != nil {
		t.Fatal(err)
	}

	if !identityPattern.MatchString(identity) || completion.Status != "success" ||
		completion.SizeBytes != int64(len(uploaded)) || completion.DurationSeconds <= 0 {
		t.Fatal("incorrect terminal metadata")
	}

	digest := sha256.Sum256(uploaded)
	if completion.Checksum != "sha256:"+hex.EncodeToString(digest[:]) || bytes.Contains(uploaded, plain) {
		t.Fatal("storage checksum/ciphertext mismatch")
	}

	var restored bytes.Buffer
	_, err = artifact.Decrypt(context.Background(), &restored, bytes.NewReader(uploaded), testKey(t))
	if err != nil || !bytes.Equal(restored.Bytes(), plain) {
		t.Fatalf("uploaded artifact cannot be restored: %v", err)
	}

	assertEmpty(t, u)
}

func TestMidstreamDisconnectRetriesIdenticalCiphertext(t *testing.T) {
	var attempts atomic.Int32
	var first, uploaded []byte
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			first = make([]byte, 512)
			if _, err := io.ReadFull(r.Body, first); err != nil {
				t.Error(err)
			}

			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}

			connection.Close()
			return
		}

		uploaded, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer storage.Close()

	p := &fakePlane{url: storage.URL}
	u := testUploader(t, p, storage, Options{})
	plain := make([]byte, 128<<10)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}

	if err := u.Run(context.Background(), io.NopCloser(bytes.NewReader(plain)), testKey(t), artifact.Gzip, nil); err != nil {
		t.Fatal(err)
	}

	if attempts.Load() != 2 || !bytes.Equal(first, uploaded[:len(first)]) ||
		len(p.reports) != 1 || p.reports[0].Status != "success" {
		t.Fatal("disconnect didn't retry the original ciphertext")
	}
	assertStable(t, p.initiations)
	assertEmpty(t, u)
}

func TestExpiredURLRenewsWithoutChangingBackup(t *testing.T) {
	for _, locallyExpired := range []bool{false, true} {
		t.Run(map[bool]string{false: "storage-rejects-signature", true: "expired-before-put"}[locallyExpired], func(t *testing.T) {
			var puts atomic.Int32
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				puts.Add(1)
				if r.URL.RawQuery == "old" {
					w.WriteHeader(403)
					return
				}

				io.Copy(io.Discard, r.Body)
				w.WriteHeader(200)
			}))
			defer storage.Close()

			p := &fakePlane{}
			p.onInitiate = func(attempt int) controlplane.InitiateBackupResponse {
				result := controlplane.InitiateBackupResponse{
					BackupID:  "backup_1",
					UploadURL: storage.URL + "?new",
					ExpiresAt: time.Now().Add(time.Hour),
				}
				if attempt == 1 {
					result.UploadURL = storage.URL + "?old"
					if locallyExpired {
						result.ExpiresAt = time.Now().Add(-time.Second)
					}
				}

				return result
			}
			u := testUploader(t, p, storage, Options{})

			if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil); err != nil {
				t.Fatal(err)
			}

			if len(p.initiations) != 2 || puts.Load() != map[bool]int32{false: 2, true: 1}[locallyExpired] {
				t.Fatal("URL wasn't renewed")
			}
			assertStable(t, p.initiations)
		})
	}
}

func TestAPIOutagePreservesCiphertextAcrossRestart(t *testing.T) {
	var puts atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		io.Copy(io.Discard, r.Body)
	}))
	defer storage.Close()

	p := &fakePlane{
		url:           storage.URL,
		initiateError: &controlplane.ResponseError{StatusCode: 503},
	}
	u := testUploader(t, p, storage, Options{})

	if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("offline backup")), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrPending) {
		t.Fatalf("not queued: %v", err)
	}

	if puts.Load() != 0 {
		t.Fatal("upload ran without initiation")
	}

	ciphertext, err := os.ReadFile(u.artifactPath())
	if err != nil {
		t.Fatal(err)
	}

	var plain bytes.Buffer
	_, err = artifact.Decrypt(context.Background(), &plain, bytes.NewReader(ciphertext), testKey(t))
	if err != nil || plain.String() != "offline backup" {
		t.Fatal("offline ciphertext not recoverable")
	}

	if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("second backup")), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrBusy) {
		t.Fatal("queued artifact was overwritten")
	}

	p.initiateError = nil
	restarted, err := New(u.dir, p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	restarted.http.Transport = storage.Client().Transport

	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	if puts.Load() != 1 || len(p.reports) != 1 || p.reports[0].Status != "success" {
		t.Fatal("queued upload wasn't resumed")
	}
	assertStable(t, p.initiations)
	assertEmpty(t, restarted)
}

func TestCompletionReportRetryIsDurableAndDoesNotReupload(t *testing.T) {
	var puts atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		io.Copy(io.Discard, r.Body)
	}))
	defer storage.Close()

	p := &fakePlane{
		url:           storage.URL,
		completeError: &controlplane.ResponseError{StatusCode: 503},
	}
	u := testUploader(t, p, storage, Options{})

	if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrPending) {
		t.Fatalf("completion not queued: %v", err)
	}

	e, err := u.load()
	if err != nil || e == nil || e.Phase != "report" || e.Completion.Status != "success" {
		t.Fatal("terminal report wasn't committed")
	}
	if _, err := os.Stat(u.artifactPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reported artifact wasn't freed")
	}

	p.completeError = nil
	restarted, _ := New(u.dir, p, Options{})

	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	if puts.Load() != 1 || len(p.initiations) != 1 ||
		len(p.reports) != 2 || !reflect.DeepEqual(p.reports[0], p.reports[1]) {
		t.Fatal("completion replay changed metadata or reuploaded")
	}
	assertEmpty(t, restarted)
}

type blockingSource struct {
	*io.PipeReader
	entered chan struct{}
	once    sync.Once
}

func (s *blockingSource) Read(p []byte) (int, error) {
	s.once.Do(func() {
		close(s.entered)
	})

	return s.PipeReader.Read(p)
}

func TestCancellationClosesBlockedSourceAndQueuesFailure(t *testing.T) {
	p := &fakePlane{url: "https://storage.example/backup"}
	u := testUploader(t, p, nil, Options{})
	reader, writer := io.Pipe()
	defer writer.Close()

	source := &blockingSource{
		PipeReader: reader,
		entered:    make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)

	go func() {
		done <- u.Run(ctx, source, testKey(t), artifact.Zstd, nil)
	}()

	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("source never started")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wrong cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation didn't unblock source")
	}

	if len(p.initiations) != 0 || len(p.reports) != 0 {
		t.Fatal("canceled context made network calls")
	}

	if err := u.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(p.reports) != 1 || p.reports[0].Status != "failed" || p.reports[0].Problem != controlplane.BackupFailed {
		t.Fatal("canceled run not reported safely")
	}
	assertEmpty(t, u)
}

func TestCancellationDuringUploadQueuesFailure(t *testing.T) {
	started := make(chan struct{})
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		io.Copy(io.Discard, r.Body)
	}))
	defer storage.Close()

	p := &fakePlane{url: storage.URL}
	u := testUploader(t, p, storage, Options{BytesPerSecond: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)

	go func() {
		done <- u.Run(ctx, io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upload never started")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wrong cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("throttled upload didn't cancel")
	}

	if err := u.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(p.reports) != 1 || p.reports[0].Status != "failed" || p.reports[0].Problem != controlplane.UploadFailed {
		t.Fatal("upload cancellation wasn't queued")
	}
	assertEmpty(t, u)
}

func TestOutageExhaustsBoundedSessionsAndFreesDisk(t *testing.T) {
	p := &fakePlane{
		initiateError: &controlplane.ResponseError{StatusCode: 503},
		url:           "https://storage.example/backup",
	}
	u := testUploader(t, p, nil, Options{})

	if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_ = u.Resume(context.Background())
	}

	e, err := u.load()
	if err != nil || e == nil || e.Phase != "report" || e.Completion.Status != "failed" {
		t.Fatal("exhausted backup didn't become a report")
	}
	if _, err := os.Stat(u.artifactPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exhausted retries kept ciphertext")
	}

	p.initiateError = nil
	if err := u.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	assertStable(t, p.initiations)
	assertEmpty(t, u)
}

func TestSpoolLimitAbortsWithoutPlaintextOrPartialUpload(t *testing.T) {
	p := &fakePlane{url: "https://storage.example/backup"}
	u := testUploader(t, p, nil, Options{MaxBytes: 50})

	err := u.Run(context.Background(), io.NopCloser(strings.NewReader("local plaintext secret")), testKey(t), artifact.Zstd, nil)

	if !errors.Is(err, ErrLimit) || !errors.Is(err, ErrBackup) {
		t.Fatalf("wrong limit error: %v", err)
	}
	if len(p.reports) != 1 || p.reports[0].Status != "failed" {
		t.Fatal("spool failure not reported")
	}
	assertEmpty(t, u)
}

func TestFreeSpaceReservePreventsWrites(t *testing.T) {
	var output bytes.Buffer
	w := &limitedWriter{
		dst:       &output,
		remaining: 100,
		reserve:   90,
		free: func() (uint64, error) {
			return 100, nil
		},
	}

	n, err := w.Write(make([]byte, 11))

	if n != 0 || !errors.Is(err, ErrLimit) || output.Len() != 0 {
		t.Fatal("reserve was consumed")
	}
}

func TestFreeSpaceIsRecheckedAsDiskHeadroomDrops(t *testing.T) {
	var output bytes.Buffer
	available := uint64(100)
	w := &limitedWriter{dst: &output, remaining: 100, reserve: 90, free: func() (uint64, error) { return available, nil }}
	if _, err := w.Write(make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	available = 92
	if n, err := w.Write(make([]byte, 5)); n != 0 || !errors.Is(err, ErrLimit) || output.Len() != 5 {
		t.Fatal("a later write consumed the free-space reserve")
	}
}

func TestExpiredArtifactBecomesFailedReportWithoutUpload(t *testing.T) {
	p := &fakePlane{
		initiateError: &controlplane.ResponseError{StatusCode: 503},
		url:           "https://storage.example/backup",
	}
	u := testUploader(t, p, nil, Options{MaxAge: time.Nanosecond})

	_ = u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil)

	p.initiateError = nil
	if err := u.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(p.reports) != 1 || p.reports[0].Status != "failed" {
		t.Fatal("expired artifact was uploaded")
	}
	assertEmpty(t, u)
}

func TestTransferSlotLockedAcrossInstances(t *testing.T) {
	u := testUploader(t, &fakePlane{}, nil, Options{})
	unlock, err := u.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other, _ := New(u.dir, &fakePlane{}, Options{})

	if err := other.Resume(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second instance accessed owned slot: %v", err)
	}
}

func TestRedirectDoesNotForwardCiphertext(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
	}))
	defer target.Close()

	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer storage.Close()

	p := &fakePlane{url: storage.URL}
	u := testUploader(t, p, storage, Options{})

	if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrUpload) {
		t.Fatalf("redirect not rejected: %v", err)
	}

	if forwarded.Load() != 0 || len(p.reports) != 1 || p.reports[0].Status != "failed" {
		t.Fatal("redirect followed or failure not reported")
	}
}

func TestCorruptLocalCiphertextCannotReportSuccess(t *testing.T) {
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer storage.Close()

	p := &fakePlane{
		url:           storage.URL,
		initiateError: &controlplane.ResponseError{StatusCode: 503},
	}
	u := testUploader(t, p, storage, Options{})

	_ = u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil)

	ciphertext, err := os.ReadFile(u.artifactPath())
	if err != nil {
		t.Fatal(err)
	}

	ciphertext[0] ^= 1
	if err := os.WriteFile(u.artifactPath(), ciphertext, 0600); err != nil {
		t.Fatal(err)
	}

	p.initiateError = nil
	if err := u.Resume(context.Background()); !errors.Is(err, ErrLocal) {
		t.Fatalf("corruption wasn't detected: %v", err)
	}

	if len(p.reports) != 1 || p.reports[0].Status != "failed" {
		t.Fatal("corrupt ciphertext reported success")
	}
	assertEmpty(t, u)
}

func TestSessionDeadlineAndUploadRetriesAreBounded(t *testing.T) {
	var puts atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(503)
	}))
	defer storage.Close()

	p := &fakePlane{url: storage.URL}
	u := testUploader(t, p, storage, Options{SessionTimeout: 50 * time.Millisecond})

	if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = u.Resume(context.Background())
	}

	if puts.Load() > 9 || len(p.reports) != 1 || p.reports[0].Status != "failed" {
		t.Fatal("sessions didn't terminate with a failure")
	}
	assertEmpty(t, u)
}

func TestRenewalCannotChangeBackupOwnership(t *testing.T) {
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer storage.Close()

	p := &fakePlane{}
	p.onInitiate = func(attempt int) controlplane.InitiateBackupResponse {
		id := "backup_1"
		if attempt > 1 {
			id = "different_backup"
		}

		return controlplane.InitiateBackupResponse{
			BackupID:  id,
			UploadURL: storage.URL,
			ExpiresAt: time.Now().Add(time.Hour),
		}
	}
	u := testUploader(t, p, storage, Options{})

	if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrLocal) {
		t.Fatalf("ownership change wasn't rejected: %v", err)
	}

	if len(p.reports) != 0 {
		t.Fatal("renewal reported completion against another backup")
	}

	e, err := u.load()
	if err != nil || e.BackupID != "backup_1" {
		t.Fatal("original binding wasn't preserved")
	}
}

func TestStorageCooldownSurvivesSessionAndRestart(t *testing.T) {
	var puts atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer storage.Close()

	p := &fakePlane{url: storage.URL}
	// Allow private state writes and the TLS exchange to finish on Windows before
	// the session expires while waiting out the one-hour storage cooldown.
	u := testUploader(t, p, storage, Options{SessionTimeout: 5 * time.Second})

	if err := u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}

	e, err := u.load()
	if err != nil || e.NotBefore.Before(time.Now().Add(59*time.Minute)) {
		t.Fatal("storage cooldown wasn't persisted")
	}

	restarted, err := New(u.dir, p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	restarted.http.Transport = storage.Client().Transport

	if err := restarted.Resume(context.Background()); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}

	if puts.Load() != 1 || len(p.initiations) != 1 {
		t.Fatal("restart ignored storage cooldown")
	}

	e, err = restarted.load()
	if err != nil || e.Sessions != 1 {
		t.Fatal("cooldown consumed another transfer session")
	}
}

func TestInterruptedEncryptionCannotUploadPartialArtifact(t *testing.T) {
	p := &fakePlane{
		initiateError: &controlplane.ResponseError{StatusCode: 503},
		url:           "https://storage.example/backup",
	}
	u := testUploader(t, p, nil, Options{})

	_ = u.Run(context.Background(), io.NopCloser(strings.NewReader("backup")), testKey(t), artifact.Zstd, nil)

	e, err := u.load()
	if err != nil {
		t.Fatal(err)
	}

	// Recreate the durable state left by a process crash before codec finalization.
	e.Phase = "building"
	e.Completion = controlplane.CompleteBackupRequest{
		Status:  "failed",
		Problem: controlplane.BackupFailed,
	}
	if err := u.save(e); err != nil {
		t.Fatal(err)
	}

	p.initiateError = nil
	restarted, _ := New(u.dir, p, Options{})

	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(p.reports) != 1 || p.reports[0].Status != "failed" || p.reports[0].Problem != controlplane.BackupFailed {
		t.Fatal("interrupted build wasn't reported as failed")
	}
	assertEmpty(t, restarted)
}

func TestUploadThrottleLimitsThroughput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	r := &uploadReader{
		ctx:     ctx,
		cancel:  cancel,
		source:  strings.NewReader(strings.Repeat("x", 100)),
		rate:    1000,
		started: started,
		digest:  sha256.New(),
	}
	defer r.Close()

	n, err := io.Copy(io.Discard, r)

	if err != nil || n != 100 {
		t.Fatalf("throttled read failed: %v", err)
	}
	if time.Since(started) < 90*time.Millisecond {
		t.Fatal("upload ignored throughput limit")
	}
}
