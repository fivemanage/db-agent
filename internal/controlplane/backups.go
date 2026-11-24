package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	MaxMultipartParts        = 10000
	MaxPartURLBatch          = 16
	multipartMaxPayloadBytes = 64 << 10
)

var (
	resourcePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	checksumPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// The orchestrator must persist this key once per run and reuse it after a
// restart; creating a fresh key per retry can create duplicate backup records.
func NewRequestID() (string, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", errors.New("cannot generate request identity")
	}
	return hex.EncodeToString(key[:]), nil
}

type MultipartPartURL struct {
	PartNumber int    `json:"part_number"`
	UploadURL  string `json:"upload_url"`
}

type CompletedPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

type InitiateBackupRequest struct {
	RequestID          string    `json:"-"`
	EstimatedSizeBytes *int64    `json:"estimated_size_bytes,omitempty"`
	StartedAt          time.Time `json:"started_at"`
	UploadMode         string    `json:"upload_mode,omitempty"`
	PartSizeBytes      int64     `json:"part_size_bytes,omitempty"`
}

type InitiateBackupResponse struct {
	BackupID      string             `json:"backup_id"`
	UploadURL     string             `json:"upload_url,omitempty"`
	UploadMode    string             `json:"upload_mode,omitempty"`
	PartSizeBytes int64              `json:"part_size_bytes,omitempty"`
	MaxSizeBytes  int64              `json:"max_size_bytes,omitempty"`
	ExpiresAt     time.Time          `json:"expires_at"`
	Parts         []MultipartPartURL `json:"parts,omitempty"`
}

type SignBackupPartsResponse struct {
	ExpiresAt time.Time          `json:"expires_at"`
	Parts     []MultipartPartURL `json:"parts"`
}

type CompleteBackupRequest struct {
	Status          string          `json:"status"`
	SizeBytes       int64           `json:"size_bytes"`
	DurationSeconds float64         `json:"duration_seconds"`
	Checksum        string          `json:"checksum,omitempty"`
	Parts           []CompletedPart `json:"parts,omitempty"`
	Problem         Problem         `json:"-"`
}

type DownloadBackupResponse struct {
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
	SizeBytes   int64     `json:"size_bytes"`
	Checksum    string    `json:"checksum"`
}

func (c *Client) InitiateBackup(ctx context.Context, input InitiateBackupRequest) (InitiateBackupResponse, error) {
	if !requestIDPattern.MatchString(input.RequestID) || input.StartedAt.IsZero() || input.EstimatedSizeBytes != nil && *input.EstimatedSizeBytes < 0 {
		return InitiateBackupResponse{}, errors.New("backup initiation requires a stable request ID, start time, and nonnegative size estimate")
	}
	if input.UploadMode != "" && input.UploadMode != "single" && input.UploadMode != "multipart" {
		return InitiateBackupResponse{}, errors.New("backup upload mode must be single or multipart")
	}
	if input.PartSizeBytes < 0 || (input.PartSizeBytes > 0 && input.UploadMode != "multipart") {
		return InitiateBackupResponse{}, errors.New("backup part size requires multipart upload mode")
	}

	maxResp := 0
	if input.UploadMode == "multipart" {
		maxResp = multipartMaxPayloadBytes
	}

	contents, err := c.request(ctx, request{
		operation:        "backup initiation",
		method:           http.MethodPost,
		path:             "/api/v1/backups/initiate",
		credential:       c.credential,
		requestID:        input.RequestID,
		body:             input,
		created:          true,
		maxResponseBytes: maxResp,
	})
	if err != nil {
		return InitiateBackupResponse{}, err
	}

	var result InitiateBackupResponse
	if json.Unmarshal(contents, &result) != nil || !resourcePattern.MatchString(result.BackupID) {
		return InitiateBackupResponse{}, errors.New("invalid backup initiation response; expected a backup ID and unexpired HTTPS upload URL")
	}
	if result.UploadMode == "multipart" {
		if result.PartSizeBytes <= 0 || result.MaxSizeBytes < 0 || !result.ExpiresAt.After(time.Now()) || !validPartURLs(result.Parts, result.ExpiresAt) {
			return InitiateBackupResponse{}, errors.New("invalid multipart backup initiation response; expected valid part size and unexpired HTTPS part URLs")
		}
		return result, nil
	}
	if result.UploadMode != "" && result.UploadMode != "single" || !validTransferURL(result.UploadURL, result.ExpiresAt) {
		return InitiateBackupResponse{}, errors.New("invalid backup initiation response; expected a backup ID and unexpired HTTPS upload URL")
	}

	return result, nil
}

func (c *Client) SignBackupParts(ctx context.Context, backupID string, partNumbers []int) (SignBackupPartsResponse, error) {
	if !resourcePattern.MatchString(backupID) || len(partNumbers) == 0 || len(partNumbers) > MaxPartURLBatch {
		return SignBackupPartsResponse{}, errors.New("backup part signing requires a valid backup ID and bounded part numbers")
	}

	requested := make(map[int]struct{}, len(partNumbers))
	for _, n := range partNumbers {
		if n < 1 || n > MaxMultipartParts {
			return SignBackupPartsResponse{}, errors.New("backup part numbers must be between 1 and 10000")
		}
		if _, dup := requested[n]; dup {
			return SignBackupPartsResponse{}, errors.New("backup part numbers must be unique")
		}
		requested[n] = struct{}{}
	}

	body := struct {
		PartNumbers []int `json:"part_numbers"`
	}{PartNumbers: partNumbers}

	contents, err := c.request(ctx, request{
		operation:        "backup part signing",
		method:           http.MethodPost,
		path:             "/api/v1/backups/" + backupID + "/parts",
		credential:       c.credential,
		body:             body,
		maxResponseBytes: multipartMaxPayloadBytes,
	})
	if err != nil {
		return SignBackupPartsResponse{}, err
	}

	var result SignBackupPartsResponse
	if json.Unmarshal(contents, &result) != nil || !result.ExpiresAt.After(time.Now()) ||
		len(result.Parts) != len(partNumbers) || !validPartURLs(result.Parts, result.ExpiresAt) {
		return SignBackupPartsResponse{}, errors.New("invalid backup part URL response")
	}
	for _, part := range result.Parts {
		if _, ok := requested[part.PartNumber]; !ok {
			return SignBackupPartsResponse{}, errors.New("backup part URL response did not match requested part numbers")
		}
	}

	return result, nil
}

func (c *Client) CompleteBackup(ctx context.Context, backupID string, input CompleteBackupRequest) error {
	if !resourcePattern.MatchString(backupID) || input.SizeBytes < 0 || input.DurationSeconds < 0 || math.IsNaN(input.DurationSeconds) || math.IsInf(input.DurationSeconds, 0) {
		return errors.New("invalid backup completion metadata")
	}

	if input.Status != "success" && input.Status != "failed" {
		return errors.New("backup completion status must be success or failed")
	}

	if input.Status == "success" && (!checksumPattern.MatchString(input.Checksum) || input.Problem != NoProblem) {
		return errors.New("successful completion requires a SHA-256 checksum and no error")
	}

	if input.Status == "failed" && (input.Checksum != "" && !checksumPattern.MatchString(input.Checksum) || len(input.Parts) > 0) {
		return errors.New("invalid backup completion metadata")
	}

	if len(input.Parts) > 0 && !ValidCompletedParts(input.Parts) {
		return errors.New("invalid multipart backup completion parts")
	}

	var message string
	if input.Status == "failed" {
		p, err := (State{Status: Error, Problem: input.Problem}).payload(c.version)
		if err != nil {
			return err
		}
		message = p.LastError
	}

	body := struct {
		CompleteBackupRequest
		Error string `json:"error,omitempty"`
	}{input, message}
	_, err := c.request(ctx,
		request{
			operation:       "backup completion",
			method:          http.MethodPost,
			path:            "/api/v1/backups/" + backupID + "/complete",
			credential:      c.credential,
			body:            body,
			maxRequestBytes: multipartMaxPayloadBytes,
		})

	return err
}

func (c *Client) DownloadBackup(ctx context.Context, backupID string) (DownloadBackupResponse, error) {
	if !resourcePattern.MatchString(backupID) {
		return DownloadBackupResponse{}, errors.New("invalid backup ID")
	}

	contents, err := c.request(ctx, request{operation: "backup download", method: http.MethodGet, path: "/api/v1/backups/" + backupID + "/download", credential: c.credential})
	if err != nil {
		return DownloadBackupResponse{}, err
	}

	var result DownloadBackupResponse
	if json.Unmarshal(contents, &result) != nil || !validTransferURL(result.DownloadURL, result.ExpiresAt) || result.SizeBytes < 85 || !checksumPattern.MatchString(result.Checksum) {
		return DownloadBackupResponse{}, errors.New("invalid backup download response; expected an unexpired HTTPS URL, encrypted size, and SHA-256 checksum")
	}

	return result, nil
}

func ValidCompletedParts(parts []CompletedPart) bool {
	if len(parts) == 0 || len(parts) > MaxMultipartParts {
		return false
	}
	for i, part := range parts {
		trimmed := strings.Trim(strings.TrimSpace(part.ETag), `"`)
		if part.PartNumber != i+1 || trimmed == "" || len(part.ETag) > 128 {
			return false
		}
	}
	return true
}

func validPartURLs(parts []MultipartPartURL, expires time.Time) bool {
	if len(parts) == 0 || len(parts) > MaxPartURLBatch {
		return false
	}
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		if part.PartNumber < 1 || part.PartNumber > MaxMultipartParts {
			return false
		}
		if _, dup := seen[part.PartNumber]; dup {
			return false
		}
		if !validTransferURL(part.UploadURL, expires) {
			return false
		}
		seen[part.PartNumber] = struct{}{}
	}
	return true
}

func validTransferURL(value string, expires time.Time) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" && expires.After(time.Now())
}
