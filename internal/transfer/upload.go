// Package transfer owns bounded ciphertext retries and durable completion reports.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/securefile"
)

var (
	ErrBusy    = errors.New("a backup or queued completion already owns the local transfer slot")
	ErrPending = errors.New("encrypted backup transfer or completion is queued for a later session")
	ErrUpload  = errors.New("encrypted backup upload failed")
	ErrLimit   = errors.New("encrypted backup exceeds the local spool or free-space limit")
	ErrLocal   = errors.New("cannot access private backup transfer state")
	ErrBackup  = errors.New("backup encryption or local staging failed")
)

// Keep causes inspectable without exposing filesystem paths or signed URLs in logs.
type safeError struct {
	message error
	cause   error
}

func (e *safeError) Error() string {
	return e.message.Error()
}

func (e *safeError) Unwrap() error {
	return e.cause
}

func (e *safeError) Is(target error) bool {
	return target == e.message
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryDelay(attempt int, retryAfter string) time.Duration {
	delay := time.Second << attempt
	delay = delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))

	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds > 0 {
		if seconds > int64((1<<63-1)/int64(time.Second)) {
			seconds = int64((1<<63 - 1) / int64(time.Second))
		}

		return max(delay, time.Duration(seconds)*time.Second)
	}
	if date, err := http.ParseTime(retryAfter); err == nil {
		return max(delay, time.Until(date))
	}

	return delay
}

func validURL(value string, expires time.Time) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" &&
		u.User == nil && u.Fragment == "" && expires.After(time.Now())
}

// put never forwards a control-plane credential, follows redirects, or echoes a
// presigned URL/error body. A retry always reopens the same complete ciphertext.
func (u *Uploader) put(ctx context.Context, entry *entry, target controlplane.InitiateBackupResponse) (bool, string, error) {
	if !validURL(target.UploadURL, target.ExpiresAt) {
		return true, "", ErrUpload
	}

	file, err := securefile.Open(u.artifactPath(), u.options.MaxBytes)
	if err != nil {
		return false, "", &safeError{ErrLocal, err}
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.Size() != entry.Completion.SizeBytes {
		return false, "", ErrLocal
	}

	bodyCtx, cancel := context.WithCancel(ctx)
	body := &uploadReader{
		ctx:     bodyCtx,
		cancel:  cancel,
		source:  file,
		rate:    u.options.BytesPerSecond,
		started: time.Now(),
		digest:  sha256.New(),
	}
	defer body.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.UploadURL, body)
	if err != nil {
		return false, "", ErrUpload
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")

	response, err := u.http.Do(req)
	// A server may reply before the request body finishes. Join/stop reads before
	// checking measurements or closing the underlying file, even on early errors.
	body.Close()
	if err != nil {
		return true, "", &safeError{ErrUpload, err}
	}
	defer response.Body.Close()

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if body.size != info.Size() || "sha256:"+hex.EncodeToString(body.digest.Sum(nil)) != entry.Completion.Checksum {
			return false, "", ErrLocal
		}

		return false, "", nil
	}

	// Providers disagree on the status for expired signatures. Re-initiation is
	// bounded and identity-preserving even when 401/403 wasn't actually expiry.
	retry := response.StatusCode == 401 || response.StatusCode == 403 ||
		response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500

	return retry, response.Header.Get("Retry-After"), ErrUpload
}

type uploadReader struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	source  io.Reader
	rate    int64
	started time.Time
	size    int64
	digest  hash.Hash
}

func (r *uploadReader) Close() error {
	r.cancel()
	r.mu.Lock()
	r.mu.Unlock()

	return nil
}

func (r *uploadReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	// Small reads bound the burst to 32 KiB; wait before releasing each chunk.
	p = p[:min(len(p), 32<<10)]
	n, err := r.source.Read(p)
	if r.rate > 0 && n > 0 {
		due := r.started.Add(time.Duration(float64(r.size+int64(n)) / float64(r.rate) * float64(time.Second)))
		if delay := time.Until(due); delay > 0 {
			if err := wait(r.ctx, delay); err != nil {
				return 0, err
			}
		}
	}

	r.size += int64(n)
	_, _ = r.digest.Write(p[:n])

	return n, err
}
