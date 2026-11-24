package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/securefile"
)

const (
	DefaultMaxBytes = int64(16 << 30)
	DefaultPartSize = int64(64 << 20)
	partURLMargin   = 2 * time.Minute
	maxPartAttempts = 3
)

type partSigner interface {
	SignBackupParts(context.Context, string, []int) (controlplane.SignBackupPartsResponse, error)
}

// streamWriter stages up to one part locally in u.artifactPath(). Backups that
// fit in a single part complete via the single-part path; larger backups spill
// to streaming multipart upload so local disk never holds more than one part.
type streamWriter struct {
	ctx          context.Context
	u            *session
	e            *entry
	file         *os.File
	partSize     int64
	maxBytes     int64
	reserve      int64
	free         func() (uint64, error)
	totalWritten int64
	buffered     int64
	multipart    bool
	nextPart     int
	completed    []controlplane.CompletedPart
	urlExpires   time.Time
	partURLs     map[int]string
	uploadErr    bool
}

func (w *streamWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if err := w.ctx.Err(); err != nil {
			return written, err
		}
		if w.totalWritten >= w.maxBytes {
			return written, ErrLimit
		}

		if w.buffered == w.partSize {
			if !w.multipart {
				if err := w.promote(); err != nil {
					return written, err
				}
				if w.totalWritten >= w.maxBytes {
					return written, ErrLimit
				}
			}
			if err := w.flushPart(); err != nil {
				return written, err
			}
		}

		spaceInPart := w.partSize - w.buffered
		remainingTotal := w.maxBytes - w.totalWritten
		chunk := min(int64(len(p)), spaceInPart, remainingTotal)

		available, err := w.free()
		if err != nil {
			return written, &safeError{ErrLocal, err}
		}
		if available < uint64(w.reserve) || uint64(chunk) > available-uint64(w.reserve) {
			return written, ErrLimit
		}

		if w.file == nil {
			file, err := securefile.OpenNew(w.u.artifactPath())
			if err != nil {
				return written, &safeError{ErrLocal, err}
			}
			w.file = file
		}

		n, err := w.file.Write(p[:chunk])
		w.buffered += int64(n)
		w.totalWritten += int64(n)
		written += n
		p = p[n:]
		if err != nil {
			return written, err
		}
	}

	return written, nil
}

func (w *streamWriter) promote() error {
	w.e.Initiation.UploadMode = "multipart"
	w.e.Initiation.PartSizeBytes = w.partSize

	target, err := w.u.plane.InitiateBackup(w.ctx, w.e.Initiation)
	if err != nil {
		w.uploadErr = true
		if errors.Is(err, controlplane.ErrCredentialRejected) {
			return err
		}
		return &safeError{ErrUpload, err}
	}

	if target.UploadMode != "multipart" || target.PartSizeBytes != w.partSize ||
		!backupPattern.MatchString(target.BackupID) || len(target.Parts) == 0 {
		w.uploadErr = true
		return ErrUpload
	}
	if target.MaxSizeBytes > 0 && target.MaxSizeBytes < w.maxBytes {
		w.maxBytes = target.MaxSizeBytes
	}

	w.e.BackupID = target.BackupID
	if err := w.u.save(w.e); err != nil {
		return err
	}

	w.multipart = true
	w.nextPart = 1
	w.urlExpires = target.ExpiresAt
	w.partURLs = make(map[int]string, len(target.Parts))
	for _, part := range target.Parts {
		w.partURLs[part.PartNumber] = part.UploadURL
	}

	return nil
}

func (w *streamWriter) closeFile() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *streamWriter) flushPart() error {
	if w.buffered <= 0 {
		return nil
	}
	if w.nextPart > controlplane.MaxMultipartParts {
		return ErrLimit
	}
	if err := w.closeFile(); err != nil {
		return &safeError{ErrLocal, err}
	}

	etag, err := w.uploadCurrentPart(w.nextPart, w.buffered)
	if err != nil {
		w.uploadErr = true
		return err
	}

	w.completed = append(w.completed, controlplane.CompletedPart{
		PartNumber: w.nextPart,
		ETag:       etag,
	})
	delete(w.partURLs, w.nextPart)
	w.nextPart++

	if err := remove(w.u.artifactPath()); err != nil {
		return err
	}
	w.buffered = 0

	return nil
}

func (w *streamWriter) maxPartNumber() int {
	total := (w.maxBytes + w.partSize - 1) / w.partSize
	if total > controlplane.MaxMultipartParts {
		return controlplane.MaxMultipartParts
	}
	if total < 1 {
		return 1
	}
	return int(total)
}

func (w *streamWriter) partURL(partNumber int, forceRenew bool) (string, error) {
	if !forceRenew && time.Until(w.urlExpires) >= partURLMargin {
		if u, ok := w.partURLs[partNumber]; ok && validURL(u, w.urlExpires) {
			return u, nil
		}
	}

	signer, ok := w.u.plane.(partSigner)
	if !ok {
		return "", ErrUpload
	}

	limit := min(partNumber+controlplane.MaxPartURLBatch-1, w.maxPartNumber())
	batch := make([]int, 0, limit-partNumber+1)
	for n := partNumber; n <= limit; n++ {
		batch = append(batch, n)
	}

	signed, err := signer.SignBackupParts(w.ctx, w.e.BackupID, batch)
	if err != nil {
		if errors.Is(err, controlplane.ErrCredentialRejected) {
			return "", err
		}
		return "", &safeError{ErrUpload, err}
	}

	w.urlExpires = signed.ExpiresAt
	w.partURLs = make(map[int]string, len(signed.Parts))
	for _, part := range signed.Parts {
		w.partURLs[part.PartNumber] = part.UploadURL
	}

	u, ok := w.partURLs[partNumber]
	if !ok || !validURL(u, w.urlExpires) {
		return "", ErrUpload
	}

	return u, nil
}

func (w *streamWriter) uploadCurrentPart(partNumber int, size int64) (string, error) {
	forceRenew := false
	for attempt := 0; attempt < maxPartAttempts; attempt++ {
		if err := w.ctx.Err(); err != nil {
			return "", err
		}

		uploadURL, err := w.partURL(partNumber, forceRenew)
		if err != nil {
			return "", err
		}
		forceRenew = false

		etag, retry, authRejected, retryAfter, err := w.u.putPart(w.ctx, size, uploadURL)
		if err == nil {
			return etag, nil
		}
		if w.ctx.Err() != nil {
			return "", w.ctx.Err()
		}
		if !retry || attempt == maxPartAttempts-1 {
			return "", err
		}
		if authRejected {
			forceRenew = true
		}

		if err := wait(w.ctx, retryDelay(attempt, retryAfter)); err != nil {
			return "", err
		}
	}

	return "", ErrUpload
}

// putPart uploads a single bounded part without forwarding agent credentials
// or following redirects. Retries reopen the same private part file.
func (u *Uploader) putPart(ctx context.Context, size int64, uploadURL string) (string, bool, bool, string, error) {
	file, err := securefile.Open(u.artifactPath(), size)
	if err != nil {
		return "", false, false, "", &safeError{ErrLocal, err}
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.Size() != size {
		return "", false, false, "", ErrLocal
	}

	bodyCtx, cancel := context.WithCancel(ctx)
	body := &uploadReader{
		ctx:     bodyCtx,
		cancel:  cancel,
		source:  io.LimitReader(file, size),
		rate:    u.options.BytesPerSecond,
		started: time.Now(),
		digest:  sha256.New(),
	}
	defer body.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return "", false, false, "", ErrUpload
	}
	req.ContentLength = size

	response, err := u.http.Do(req)
	body.Close()
	if err != nil {
		return "", true, false, "", &safeError{ErrUpload, err}
	}
	defer response.Body.Close()

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		etag := strings.Trim(strings.TrimSpace(response.Header.Get("ETag")), `"`)
		if body.size != size || etag == "" || len(etag) > 128 {
			return "", false, false, "", ErrUpload
		}
		return etag, false, false, "", nil
	}

	authRejected := response.StatusCode == 401 || response.StatusCode == 403
	retry := authRejected || response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500

	return "", retry, authRejected, response.Header.Get("Retry-After"), ErrUpload
}
