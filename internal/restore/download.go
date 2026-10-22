package restore

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
)

var checksumPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (r *Restorer) download(ctx context.Context, id, output string, k key.Key) (Result, error) {
	var expected *controlplane.DownloadBackupResponse
	for attempt := 0; attempt < 3; attempt++ {
		target, err := r.lookup.DownloadBackup(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			if errors.Is(err, controlplane.ErrCredentialRejected) {
				return Result{}, controlplane.ErrCredentialRejected
			}
			return Result{}, &safeError{ErrDownload, err}
		}
		if !validDownload(target) {
			return Result{}, ErrDownload
		}
		if target.SizeBytes > r.options.MaxInputBytes {
			return Result{}, ErrLimit
		}
		if expected == nil {
			expected = &target
		} else if target.Checksum != expected.Checksum || target.SizeBytes != expected.SizeBytes {
			// URL renewal may not silently switch to a different artifact.
			return Result{}, ErrIntegrity
		}

		result, retry, retryAfter, err := r.fetch(ctx, output, k, target)
		if err == nil {
			// Once verified output is committed, a later cancellation must not
			// report failure and cause callers to retry an already completed restore.
			return result, nil
		}
		if ctx.Err() != nil {
			if errors.Is(err, ctx.Err()) {
				return Result{}, err
			}
			return Result{}, errors.Join(ctx.Err(), err)
		}
		if !retry || attempt == 2 {
			return result, err
		}
		if err := r.wait(ctx, retryDelay(attempt, retryAfter)); err != nil {
			return Result{}, err
		}
	}
	return Result{}, ErrDownload
}

func validDownload(target controlplane.DownloadBackupResponse) bool {
	u, err := url.Parse(target.DownloadURL)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" &&
		target.ExpiresAt.After(time.Now()) && target.SizeBytes >= 85 && checksumPattern.MatchString(target.Checksum)
}

func (r *Restorer) fetch(ctx context.Context, output string, k key.Key, target controlplane.DownloadBackupResponse) (Result, bool, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.DownloadURL, nil)
	if err != nil {
		return Result{}, false, "", ErrDownload
	}
	// Keep storage requests independent of agent authentication and disable HTTP
	// content decoding: trusted metadata hashes the exact encrypted object bytes.
	request.Header.Set("Accept-Encoding", "identity")
	response, err := r.http.Do(request)
	if err != nil {
		return Result{}, true, "", &safeError{ErrDownload, err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		retry := response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500
		return Result{}, retry, response.Header.Get("Retry-After"), ErrDownload
	}
	if response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" || response.ContentLength >= 0 && response.ContentLength != target.SizeBytes {
		return Result{}, false, "", ErrIntegrity
	}
	input := &trackedReader{source: response.Body}
	result, err := r.decode(ctx, output, input, k, &target)
	retry := errors.Is(err, ErrIntegrity) && !errors.Is(err, ErrCleanup) &&
		!errors.Is(err, artifact.ErrAuthentication) && !errors.Is(err, artifact.ErrCorrupt) &&
		(errors.Is(err, artifact.ErrTruncated) || input.err != nil && !errors.Is(input.err, io.EOF))
	return result, retry, "", err
}

type trackedReader struct {
	source io.Reader
	err    error
}

func (r *trackedReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if err != nil {
		r.err = err
	}
	return n, err
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

func retryDelay(attempt int, value string) time.Duration {
	delay := time.Duration(1<<attempt) * (500*time.Millisecond + time.Duration(rand.Int64N(int64(500*time.Millisecond))))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		seconds = min(seconds, int64((1<<63-1)/int64(time.Second)))
		return max(delay, time.Duration(seconds)*time.Second)
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(delay, time.Until(date))
	}
	return delay
}
