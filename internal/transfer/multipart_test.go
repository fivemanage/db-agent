package transfer

import (
	"bytes"
	"context"
	"crypto/md5"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
)

func TestMultipartStreamingUploadBatchesPartsAndRestoresSQL(t *testing.T) {
	// 18 parts at 8 KiB each (> MaxPartURLBatch=16) forces both initial signing
	// and a follow-up /parts batch while keeping local disk bounded to one part.
	const partSize = int64(8 << 10)
	plain := make([]byte, 140<<10)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	storedParts := make(map[int][]byte)
	var maxSpoolBytes int64
	var uploader *Uploader

	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "" {
			t.Error("invalid part upload request or forwarded credential")
		}
		partNum, err := strconv.Atoi(r.URL.Query().Get("part"))
		if err != nil || partNum < 1 {
			t.Errorf("missing part number: %s", r.URL.RawQuery)
			w.WriteHeader(400)
			return
		}
		if uploader != nil {
			if info, err := os.Stat(uploader.artifactPath()); err == nil {
				mu.Lock()
				if info.Size() > maxSpoolBytes {
					maxSpoolBytes = info.Size()
				}
				mu.Unlock()
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		sum := md5.Sum(body)
		mu.Lock()
		storedParts[partNum] = body
		mu.Unlock()
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(200)
	}))
	defer storage.Close()

	var partSignCalls atomic.Int32
	var completion controlplane.CompleteBackupRequest
	credential := "dac_" + strings.Repeat("b", 43)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("missing agent authentication")
		}
		expires := time.Now().Add(15 * time.Minute).UTC()

		switch r.URL.Path {
		case "/api/v1/backups/initiate":
			var in controlplane.InitiateBackupRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.UploadMode != "multipart" || in.PartSizeBytes != partSize {
				t.Errorf("unexpected initiate request: %+v %v", in, err)
			}
			parts := make([]controlplane.MultipartPartURL, 0, controlplane.MaxPartURLBatch)
			for i := 1; i <= controlplane.MaxPartURLBatch; i++ {
				parts = append(parts, controlplane.MultipartPartURL{
					PartNumber: i,
					UploadURL:  storage.URL + "/part?part=" + strconv.Itoa(i),
				})
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(controlplane.InitiateBackupResponse{
				BackupID:      "backup_mp_1",
				UploadMode:    "multipart",
				PartSizeBytes: partSize,
				MaxSizeBytes:  16 << 30,
				ExpiresAt:     expires,
				Parts:         parts,
			})
		case "/api/v1/backups/backup_mp_1/parts":
			partSignCalls.Add(1)
			var in struct {
				PartNumbers []int `json:"part_numbers"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.PartNumbers) == 0 {
				t.Errorf("unexpected parts request: %+v %v", in, err)
			}
			parts := make([]controlplane.MultipartPartURL, 0, len(in.PartNumbers))
			for _, n := range in.PartNumbers {
				parts = append(parts, controlplane.MultipartPartURL{
					PartNumber: n,
					UploadURL:  storage.URL + "/part?part=" + strconv.Itoa(n),
				})
			}
			json.NewEncoder(w).Encode(controlplane.SignBackupPartsResponse{
				ExpiresAt: expires,
				Parts:     parts,
			})
		case "/api/v1/backups/backup_mp_1/complete":
			if err := json.NewDecoder(r.Body).Decode(&completion); err != nil {
				t.Error(err)
			}
			io.WriteString(w, "{}")
		default:
			t.Errorf("unexpected API call: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()

	client, err := controlplane.New(api.URL, credential, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	uploader = testUploader(t, client, storage, Options{PartSize: partSize})

	if err := uploader.Run(context.Background(), io.NopCloser(bytes.NewReader(plain)), testKey(t), artifact.Zstd, nil); err != nil {
		t.Fatalf("multipart run failed: %v", err)
	}

	if completion.Status != "success" || len(completion.Parts) <= controlplane.MaxPartURLBatch {
		t.Fatalf("expected >%d parts, got status=%s parts=%d", controlplane.MaxPartURLBatch, completion.Status, len(completion.Parts))
	}
	if partSignCalls.Load() < 1 {
		t.Fatal("expected follow-up /parts batch request")
	}
	if maxSpoolBytes > partSize {
		t.Fatalf("local part spool exceeded single part size: got %d > %d", maxSpoolBytes, partSize)
	}

	var assembled bytes.Buffer
	for i, part := range completion.Parts {
		if part.PartNumber != i+1 {
			t.Fatalf("non-contiguous part number %d at index %d", part.PartNumber, i)
		}
		chunk := storedParts[part.PartNumber]
		if i < len(completion.Parts)-1 && int64(len(chunk)) != partSize {
			t.Fatalf("non-last part %d had size %d, want %d", part.PartNumber, len(chunk), partSize)
		}
		if i == len(completion.Parts)-1 && (len(chunk) == 0 || int64(len(chunk)) > partSize) {
			t.Fatalf("last part %d had invalid size %d", part.PartNumber, len(chunk))
		}
		assembled.Write(chunk)
	}

	if int64(assembled.Len()) != completion.SizeBytes {
		t.Fatalf("assembled size %d != completion size %d", assembled.Len(), completion.SizeBytes)
	}
	sum := sha256.Sum256(assembled.Bytes())
	if completion.Checksum != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("assembled multipart ciphertext checksum mismatch")
	}

	var restored bytes.Buffer
	if _, err := artifact.Decrypt(context.Background(), &restored, bytes.NewReader(assembled.Bytes()), testKey(t)); err != nil {
		t.Fatalf("cannot decrypt assembled multipart artifact: %v", err)
	}
	if !bytes.Equal(restored.Bytes(), plain) {
		t.Fatal("decrypted multipart SQL does not match original input")
	}
	assertEmpty(t, uploader)
}

func TestMultipartPartRetryAndURLRenewalOn403(t *testing.T) {
	const partSize = int64(8 << 10)
	plain := make([]byte, 24<<10)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}

	var part2Attempts atomic.Int32
	var renewCalls atomic.Int32
	var mu sync.Mutex
	storedParts := make(map[int][]byte)

	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		partNum, _ := strconv.Atoi(r.URL.Query().Get("part"))
		if partNum == 2 && r.URL.Query().Get("gen") == "1" {
			part2Attempts.Add(1)
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(403)
			return
		}
		if partNum == 2 {
			part2Attempts.Add(1)
		}
		body, _ := io.ReadAll(r.Body)
		sum := md5.Sum(body)
		mu.Lock()
		storedParts[partNum] = body
		mu.Unlock()
		w.Header().Set("ETag", hex.EncodeToString(sum[:]))
		w.WriteHeader(200)
	}))
	defer storage.Close()

	var completion controlplane.CompleteBackupRequest
	credential := "dac_" + strings.Repeat("c", 43)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expires := time.Now().Add(15 * time.Minute).UTC()
		switch r.URL.Path {
		case "/api/v1/backups/initiate":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(controlplane.InitiateBackupResponse{
				BackupID:      "backup_mp_retry",
				UploadMode:    "multipart",
				PartSizeBytes: partSize,
				MaxSizeBytes:  16 << 30,
				ExpiresAt:     expires,
				Parts: []controlplane.MultipartPartURL{
					{PartNumber: 1, UploadURL: storage.URL + "/p?part=1&gen=1"},
					{PartNumber: 2, UploadURL: storage.URL + "/p?part=2&gen=1"},
					{PartNumber: 3, UploadURL: storage.URL + "/p?part=3&gen=1"},
					{PartNumber: 4, UploadURL: storage.URL + "/p?part=4&gen=1"},
				},
			})
		case "/api/v1/backups/backup_mp_retry/parts":
			renewCalls.Add(1)
			var in struct {
				PartNumbers []int `json:"part_numbers"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			parts := make([]controlplane.MultipartPartURL, 0, len(in.PartNumbers))
			for _, n := range in.PartNumbers {
				parts = append(parts, controlplane.MultipartPartURL{
					PartNumber: n,
					UploadURL:  storage.URL + "/p?part=" + strconv.Itoa(n) + "&gen=2",
				})
			}
			json.NewEncoder(w).Encode(controlplane.SignBackupPartsResponse{
				ExpiresAt: expires,
				Parts:     parts,
			})
		case "/api/v1/backups/backup_mp_retry/complete":
			json.NewDecoder(r.Body).Decode(&completion)
			io.WriteString(w, "{}")
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()

	client, err := controlplane.New(api.URL, credential, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	u := testUploader(t, client, storage, Options{PartSize: partSize})

	if err := u.Run(context.Background(), io.NopCloser(bytes.NewReader(plain)), testKey(t), artifact.Zstd, nil); err != nil {
		t.Fatalf("expected renewal to recover from 403, got: %v", err)
	}
	if part2Attempts.Load() != 2 || renewCalls.Load() != 1 || completion.Status != "success" {
		t.Fatalf("unexpected retry state: part2Attempts=%d renewCalls=%d status=%s", part2Attempts.Load(), renewCalls.Load(), completion.Status)
	}
	assertEmpty(t, u)
}

func TestMultipartCompletionRetryPreservesPartsWithoutReuploading(t *testing.T) {
	const partSize = int64(8 << 10)
	plain := make([]byte, 24<<10)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}

	var putCalls atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		sum := md5.Sum(body)
		w.Header().Set("ETag", hex.EncodeToString(sum[:]))
		w.WriteHeader(200)
	}))
	defer storage.Close()

	var failComplete atomic.Bool
	failComplete.Store(true)
	var mu sync.Mutex
	var reports []controlplane.CompleteBackupRequest
	credential := "dac_" + strings.Repeat("d", 43)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expires := time.Now().Add(15 * time.Minute).UTC()
		switch r.URL.Path {
		case "/api/v1/backups/initiate":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(controlplane.InitiateBackupResponse{
				BackupID:      "backup_mp_comp",
				UploadMode:    "multipart",
				PartSizeBytes: partSize,
				MaxSizeBytes:  16 << 30,
				ExpiresAt:     expires,
				Parts: []controlplane.MultipartPartURL{
					{PartNumber: 1, UploadURL: storage.URL + "/p?part=1"},
					{PartNumber: 2, UploadURL: storage.URL + "/p?part=2"},
					{PartNumber: 3, UploadURL: storage.URL + "/p?part=3"},
					{PartNumber: 4, UploadURL: storage.URL + "/p?part=4"},
				},
			})
		case "/api/v1/backups/backup_mp_comp/complete":
			var in controlplane.CompleteBackupRequest
			json.NewDecoder(r.Body).Decode(&in)
			if failComplete.Load() {
				w.WriteHeader(503)
				return
			}
			mu.Lock()
			reports = append(reports, in)
			mu.Unlock()
			io.WriteString(w, "{}")
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()

	client, err := controlplane.New(api.URL, credential, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	u := testUploader(t, client, storage, Options{PartSize: partSize})

	if err := u.Run(context.Background(), io.NopCloser(bytes.NewReader(plain)), testKey(t), artifact.Zstd, nil); !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending on completion 503, got: %v", err)
	}
	initialPuts := putCalls.Load()

	e, err := u.load()
	if err != nil || e == nil || e.Phase != "report" || e.Completion.Status != "success" || len(e.Completion.Parts) < 2 {
		t.Fatalf("multipart completion report was not persisted: %+v %v", e, err)
	}
	if _, err := os.Stat(u.artifactPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("part buffer was not removed after upload finished")
	}

	failComplete.Store(false)
	restarted, err := New(u.dir, client, Options{PartSize: partSize})
	if err != nil {
		t.Fatal(err)
	}
	restarted.http.Transport = storage.Client().Transport

	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if putCalls.Load() != initialPuts || len(reports) != 1 || !reflect.DeepEqual(reports[0].Parts, e.Completion.Parts) {
		t.Fatal("completion replay reuploaded parts or altered part ETags")
	}
	assertEmpty(t, restarted)
}

func TestMultipartFailureAndCancellationReportFailedWithBackupID(t *testing.T) {
	const partSize = int64(8 << 10)
	plain := make([]byte, 24<<10)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}

	startedPart2 := make(chan struct{})
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		partNum, _ := strconv.Atoi(r.URL.Query().Get("part"))
		if partNum == 2 {
			close(startedPart2)
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(200)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sum := md5.Sum(body)
		w.Header().Set("ETag", hex.EncodeToString(sum[:]))
		w.WriteHeader(200)
	}))
	defer storage.Close()

	var mu sync.Mutex
	var completedID string
	var completedReq controlplane.CompleteBackupRequest
	credential := "dac_" + strings.Repeat("e", 43)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expires := time.Now().Add(15 * time.Minute).UTC()
		switch r.URL.Path {
		case "/api/v1/backups/initiate":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(controlplane.InitiateBackupResponse{
				BackupID:      "backup_mp_cancel",
				UploadMode:    "multipart",
				PartSizeBytes: partSize,
				MaxSizeBytes:  16 << 30,
				ExpiresAt:     expires,
				Parts: []controlplane.MultipartPartURL{
					{PartNumber: 1, UploadURL: storage.URL + "/p?part=1"},
					{PartNumber: 2, UploadURL: storage.URL + "/p?part=2"},
					{PartNumber: 3, UploadURL: storage.URL + "/p?part=3"},
					{PartNumber: 4, UploadURL: storage.URL + "/p?part=4"},
				},
			})
		case "/api/v1/backups/backup_mp_cancel/complete":
			var in controlplane.CompleteBackupRequest
			json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			completedID = "backup_mp_cancel"
			completedReq = in
			mu.Unlock()
			io.WriteString(w, "{}")
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()

	client, err := controlplane.New(api.URL, credential, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	u := testUploader(t, client, storage, Options{PartSize: partSize, BytesPerSecond: 16 << 10})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- u.Run(ctx, io.NopCloser(bytes.NewReader(plain)), testKey(t), artifact.Zstd, nil)
	}()

	select {
	case <-startedPart2:
	case <-time.After(5 * time.Second):
		t.Fatal("part 2 upload never started")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("throttled multipart upload did not cancel promptly")
	}

	if err := u.Resume(context.Background()); err != nil {
		t.Fatalf("resume failed to deliver failed report: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if completedID != "backup_mp_cancel" || completedReq.Status != "failed" || len(completedReq.Parts) != 0 {
		t.Fatalf("canceled multipart backup was not reported failed with original backup ID: id=%s req=%+v", completedID, completedReq)
	}
	assertEmpty(t, u)
}
