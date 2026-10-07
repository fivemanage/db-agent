package controlplane

import (
	"context"
	"errors"
	"time"
)

const HeartbeatInterval = 60 * time.Second

type Status string

const (
	Idle      Status = "idle"
	BackingUp Status = "backing_up"
	Uploading Status = "uploading"
	Error     Status = "error"
)

// Problems are classified messages, never raw DB errors, dump output, paths,
// credentials, encryption keys, or presigned URLs from a failed operation.
type Problem string

const (
	NoProblem           Problem = ""
	SetupIncomplete     Problem = "setup_incomplete"
	DatabaseUnavailable Problem = "database_unavailable"
	BackupFailed        Problem = "backup_failed"
	UploadFailed        Problem = "upload_failed"
)

type State struct {
	Status  Status
	Problem Problem
	DatabaseMetadata
}

type heartbeatRequest struct {
	AgentVersion string `json:"agent_version"`
	Status       Status `json:"status"`
	LastError    string `json:"last_error,omitempty"`
	DatabaseMetadata
}

func (s State) payload(version string) (heartbeatRequest, error) {
	if err := s.DatabaseMetadata.validate(); err != nil {
		return heartbeatRequest{}, err
	}
	payload := heartbeatRequest{AgentVersion: version, Status: s.Status, DatabaseMetadata: s.DatabaseMetadata}
	switch s.Status {
	case Idle, BackingUp, Uploading, Error:
	default:
		return heartbeatRequest{}, errors.New("invalid heartbeat activity state")
	}
	if s.Status != Error && s.Problem != NoProblem {
		return heartbeatRequest{}, errors.New("only an error heartbeat may report a problem")
	}
	switch s.Problem {
	case NoProblem:
		if s.Status == Error {
			payload.LastError = "Agent reports a local error; check local logs and run db-agent test."
		}
	case SetupIncomplete:
		payload.LastError = "Local backup prerequisites are incomplete; run db-agent configure and db-agent test. No backup was executed."
	case DatabaseUnavailable:
		payload.LastError = "Local database connection failed; check the database service and run db-agent test."
	case BackupFailed:
		payload.LastError = "Backup failed; check local agent logs and backup prerequisites before retrying."
	case UploadFailed:
		payload.LastError = "Encrypted backup upload failed; check outbound network access and local agent logs before retrying."
	default:
		return heartbeatRequest{}, errors.New("heartbeat problems must be classified; raw error messages are not accepted")
	}
	return payload, nil
}

type Connection string

const (
	Connected   Connection = "connected"
	Unreachable Connection = "unreachable"
	Rejected    Connection = "rejected"
	Failed      Connection = "failed"
)

type Result struct {
	Connection Connection
	Err        error
}

// RunHeartbeat samples current before each poll and calls report synchronously.
// A source updated by other workers must be concurrency-safe.
func (c *Client) RunHeartbeat(ctx context.Context, current func() State, report func(Result)) error {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	return c.runHeartbeat(ctx, current, report, ticker.C)
}

func (c *Client) runHeartbeat(ctx context.Context, current func() State, report func(Result), ticks <-chan time.Time) error {
	if current == nil {
		return errors.New("heartbeat requires a current-state source")
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := c.Heartbeat(ctx, current())
		if ctx.Err() != nil {
			return nil
		}
		result := Result{Connection: Connected, Err: err}
		if err != nil {
			result.Connection = Unreachable
			if errors.Is(err, ErrCredentialRejected) {
				result.Connection = Rejected
			}
			if !retryable(err) && result.Connection != Rejected {
				result.Connection = Failed
			}
		}
		if report != nil {
			report(result)
		}
		if err != nil {
			if !retryable(err) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok {
				return nil
			}
		}
	}
}

func retryable(err error) bool {
	var response *ResponseError
	var transport *TransportError
	return errors.As(err, &transport) || errors.As(err, &response) && (response.StatusCode == 429 || response.StatusCode >= 500)
}
