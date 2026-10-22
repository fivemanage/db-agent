// Package restorejob coordinates local restore execution and records intent and
// outcomes before they can be acknowledged remotely.
package restorejob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
)

var (
	ErrBinding    = errors.New("restore job does not match this database and agent")
	ErrConflict   = errors.New("restore job identity conflicts with its saved request; no import was started")
	ErrInvalid    = errors.New("restore job requires bounded IDs and trusted encrypted size and checksum")
	ErrState      = errors.New("cannot access private restore job state; stop restore work and inspect local state before retrying")
	ErrTransition = errors.New("restore job transition is not allowed; no additional import was authorized")
	ErrClosed     = errors.New("restore job session is closed")
)

var (
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	checksumPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Binding struct {
	DatabaseID string `json:"database_id"`
	AgentID    string `json:"agent_id"`
}

type Job struct {
	ID string `json:"id"`
	Binding
	BackupID  string `json:"backup_id"`
	SizeBytes int64  `json:"size_bytes"`
	Checksum  string `json:"checksum"`
}

func (b Binding) valid() bool {
	return idPattern.MatchString(b.DatabaseID) && idPattern.MatchString(b.AgentID)
}

func (j Job) valid() bool {
	return j.Binding.valid() && idPattern.MatchString(j.ID) && idPattern.MatchString(j.BackupID) &&
		j.SizeBytes >= 85 && checksumPattern.MatchString(j.Checksum)
}

// Destination is only a proposed name, not permission to create or reuse it.
// The importer must separately prove that this database is new and isolated.
func (j Job) Destination() string {
	digest := sha256.Sum256([]byte(j.DatabaseID + "/" + j.AgentID + "/" + j.ID))
	return "fm_restore_" + hex.EncodeToString(digest[:16])
}

type Phase string

const (
	Accepted    Phase = "accepted"
	Downloading Phase = "downloading"
	Verified    Phase = "verified"
	Importing   Phase = "importing"
	Validating  Phase = "validating"
	Succeeded   Phase = "succeeded"
	Failed      Phase = "failed"
	Canceled    Phase = "canceled"
)

type Problem string

const (
	NoProblem                     Problem = ""
	OriginalKeyUnavailable        Problem = "original_key_unavailable"
	WriteAuthorizationUnavailable Problem = "write_authorization_unavailable"
	UnsafeImport                  Problem = "unsafe_import"
	ArtifactInvalid               Problem = "artifact_invalid"
	DownloadFailed                Problem = "download_failed"
	ResourceLimit                 Problem = "resource_limit"
	ImportFailed                  Problem = "import_failed"
	ValidationFailed              Problem = "validation_failed"
	ImportInterrupted             Problem = "import_interrupted"
	LocalUnavailable              Problem = "local_restore_unavailable"
)

func (p Problem) valid() bool {
	switch p {
	case OriginalKeyUnavailable, WriteAuthorizationUnavailable, UnsafeImport, ArtifactInvalid,
		DownloadFailed, ResourceLimit, ImportFailed, ValidationFailed, ImportInterrupted, LocalUnavailable:
		return true
	default:
		return false
	}
}

type State struct {
	Version              int     `json:"version"`
	Job                  Job     `json:"job"`
	Phase                Phase   `json:"phase"`
	Revision             uint64  `json:"revision"`
	AcknowledgedRevision uint64  `json:"acknowledged_revision"`
	WritesStarted        bool    `json:"writes_started"`
	Problem              Problem `json:"problem,omitempty"`
}

func (s State) Terminal() bool {
	return s.Phase == Succeeded || s.Phase == Failed || s.Phase == Canceled
}

func (s State) Pending() bool { return s.AcknowledgedRevision < s.Revision }

func (s State) valid() bool {
	if s.Version != 1 || !s.Job.valid() || s.Revision == 0 || s.AcknowledgedRevision > s.Revision {
		return false
	}
	if s.Phase == Failed {
		if !s.Problem.valid() {
			return false
		}
		if s.Problem == ImportInterrupted || s.Problem == ImportFailed || s.Problem == ValidationFailed {
			return s.WritesStarted
		}
		return true
	}
	if s.Problem != NoProblem {
		return false
	}
	switch s.Phase {
	case Accepted, Downloading, Verified:
		return !s.WritesStarted
	case Importing, Validating, Succeeded:
		return s.WritesStarted
	case Canceled:
		return true
	default:
		return false
	}
}
