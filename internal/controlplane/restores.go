package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

const RestoreProtocol = 1

type RestorePollRequest struct {
	Protocol    int    `json:"protocol"`
	Readiness   string `json:"readiness"`
	ActiveJobID string `json:"active_job_id,omitempty"`
}

type RestoreJob struct {
	ID         string `json:"id"`
	DatabaseID string `json:"database_id"`
	AgentID    string `json:"agent_id"`
	BackupID   string `json:"backup_id"`
	SizeBytes  int64  `json:"size_bytes"`
	Checksum   string `json:"checksum"`
	Revision   uint64 `json:"revision"`
}

type RestorePollResponse struct {
	Protocol        int         `json:"protocol"`
	DatabaseID      string      `json:"database_id"`
	AgentID         string      `json:"agent_id"`
	Job             *RestoreJob `json:"job"`
	CancelRequested bool        `json:"cancel_requested"`
}

type RestoreReport struct {
	Revision      uint64 `json:"revision"`
	Phase         string `json:"phase"`
	WritesStarted bool   `json:"writes_started"`
	Problem       string `json:"problem,omitempty"`
}

type RestoreReportResponse struct {
	Revision        uint64 `json:"revision"`
	CancelRequested bool   `json:"cancel_requested"`
}

func validRestoreReadiness(value string) bool {
	switch value {
	case "ready", "original_key_unavailable", "write_authorization_unavailable", "unsafe_import", "local_restore_unavailable":
		return true
	}
	return false
}

func (c *Client) PollRestores(ctx context.Context, input RestorePollRequest) (RestorePollResponse, error) {
	if input.Protocol != RestoreProtocol || !validRestoreReadiness(input.Readiness) || input.ActiveJobID != "" && !resourcePattern.MatchString(input.ActiveJobID) {
		return RestorePollResponse{}, errors.New("invalid restore poll input; only classified readiness and a stable job ID are accepted")
	}
	data, err := c.request(ctx, request{operation: "restore poll", method: http.MethodPost, path: "/api/v1/agent/restores/poll", credential: c.credential, body: input})
	if err != nil {
		return RestorePollResponse{}, err
	}
	var result RestorePollResponse
	if json.Unmarshal(data, &result) != nil || result.Protocol != RestoreProtocol || !resourcePattern.MatchString(result.DatabaseID) || !resourcePattern.MatchString(result.AgentID) {
		return RestorePollResponse{}, errors.New("invalid authenticated restore poll response")
	}
	if result.Job != nil {
		job := result.Job
		if !resourcePattern.MatchString(job.ID) || !resourcePattern.MatchString(job.BackupID) || job.DatabaseID != result.DatabaseID || job.AgentID != result.AgentID || job.SizeBytes < 85 || !checksumPattern.MatchString(job.Checksum) || job.Revision > 2147483647 || input.ActiveJobID != "" && job.ID != input.ActiveJobID {
			return RestorePollResponse{}, errors.New("restore delivery does not match the authenticated agent/database/artifact binding")
		}
	} else if input.ActiveJobID != "" || result.CancelRequested {
		return RestorePollResponse{}, errors.New("restore poll lost the active bound job")
	}
	return result, nil
}

func (c *Client) ReportRestore(ctx context.Context, jobID string, input RestoreReport) (RestoreReportResponse, error) {
	if !resourcePattern.MatchString(jobID) || input.Revision == 0 || input.Revision > 2147483647 {
		return RestoreReportResponse{}, errors.New("restore progress requires a stable job and positive bounded revision")
	}
	// Never accept arbitrary diagnostic text as a report problem.
	switch input.Problem {
	case "", "original_key_unavailable", "write_authorization_unavailable", "unsafe_import", "artifact_invalid", "download_failed", "resource_limit", "import_failed", "validation_failed", "import_interrupted", "local_restore_unavailable":
	default:
		return RestoreReportResponse{}, errors.New("restore problems must be classified")
	}
	switch input.Phase {
	case "accepted", "downloading", "verified":
		if input.WritesStarted || input.Problem != "" {
			return RestoreReportResponse{}, errors.New("invalid pre-import restore progress")
		}
	case "importing", "validating", "succeeded":
		if !input.WritesStarted || input.Problem != "" {
			return RestoreReportResponse{}, errors.New("invalid restore write progress")
		}
	case "failed":
		if input.Problem == "" {
			return RestoreReportResponse{}, errors.New("failed restore requires a classified problem")
		}
	case "canceled":
		if input.Problem != "" {
			return RestoreReportResponse{}, errors.New("canceled restore cannot report arbitrary errors")
		}
	default:
		return RestoreReportResponse{}, errors.New("invalid restore phase")
	}
	data, err := c.request(ctx, request{operation: "restore report", method: http.MethodPost, path: "/api/v1/agent/restores/" + jobID + "/report", credential: c.credential, body: input})
	if err != nil {
		return RestoreReportResponse{}, err
	}
	var result RestoreReportResponse
	if json.Unmarshal(data, &result) != nil || result.Revision != input.Revision {
		return RestoreReportResponse{}, errors.New("restore report response did not acknowledge the exact revision")
	}
	return result, nil
}
