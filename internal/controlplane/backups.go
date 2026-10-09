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
	"time"
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

type InitiateBackupRequest struct {
	RequestID          string    `json:"-"`
	EstimatedSizeBytes *int64    `json:"estimated_size_bytes,omitempty"`
	StartedAt          time.Time `json:"started_at"`
}

type InitiateBackupResponse struct {
	BackupID  string    `json:"backup_id"`
	UploadURL string    `json:"upload_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CompleteBackupRequest struct {
	Status          string  `json:"status"`
	SizeBytes       int64   `json:"size_bytes"`
	DurationSeconds float64 `json:"duration_seconds"`
	Checksum        string  `json:"checksum,omitempty"`
	Problem         Problem `json:"-"`
}

type DownloadBackupResponse struct {
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (c *Client) InitiateBackup(ctx context.Context, input InitiateBackupRequest) (InitiateBackupResponse, error) {
	if !requestIDPattern.MatchString(input.RequestID) || input.StartedAt.IsZero() || input.EstimatedSizeBytes != nil && *input.EstimatedSizeBytes < 0 {
		return InitiateBackupResponse{}, errors.New("backup initiation requires a stable request ID, start time, and nonnegative size estimate")
	}

	contents, err := c.request(ctx, request{operation: "backup initiation", method: http.MethodPost, path: "/v1/backups/initiate", credential: c.credential, requestID: input.RequestID, body: input, created: true})
	if err != nil {
		return InitiateBackupResponse{}, err
	}

	var result InitiateBackupResponse
	if json.Unmarshal(contents, &result) != nil || !resourcePattern.MatchString(result.BackupID) || !validTransferURL(result.UploadURL, result.ExpiresAt) {
		return InitiateBackupResponse{}, errors.New("invalid backup initiation response; expected a backup ID and unexpired HTTPS upload URL")
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

	if input.Status == "failed" && input.Checksum != "" && !checksumPattern.MatchString(input.Checksum) {
		return errors.New("invalid backup checksum")
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
			operation:  "backup completion",
			method:     http.MethodPost,
			path:       "/v1/backups/" + backupID + "/complete",
			credential: c.credential, body: body,
		})

	return err
}

func (c *Client) DownloadBackup(ctx context.Context, backupID string) (DownloadBackupResponse, error) {
	if !resourcePattern.MatchString(backupID) {
		return DownloadBackupResponse{}, errors.New("invalid backup ID")
	}

	contents, err := c.request(ctx, request{operation: "backup download", method: http.MethodGet, path: "/v1/backups/" + backupID + "/download", credential: c.credential})
	if err != nil {
		return DownloadBackupResponse{}, err
	}

	var result DownloadBackupResponse
	if json.Unmarshal(contents, &result) != nil || !validTransferURL(result.DownloadURL, result.ExpiresAt) {
		return DownloadBackupResponse{}, errors.New("invalid backup download response; expected an unexpired HTTPS URL")
	}

	return result, nil
}

func validTransferURL(value string, expires time.Time) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" && expires.After(time.Now())
}
