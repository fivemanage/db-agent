// Package restore recovers authenticated SQL locally without connecting to a database.
package restore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fivemanage/db-agent/internal/artifact"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/securefile"
)

const (
	DefaultTimeout        = 2 * time.Hour
	DefaultMaxOutputBytes = int64(16 << 30)
	DefaultMaxInputBytes  = int64(1 << 30)
	DefaultReserveBytes   = uint64(256 << 20)
)

var (
	ErrExists    = errors.New("restore output already exists; choose a different output file; nothing was overwritten")
	ErrOutput    = errors.New("cannot write private restore output; check the destination directory, permissions, and available disk space")
	ErrLimit     = errors.New("restore exceeds the size limit or free disk space reserve; choose a larger disk or explicitly increase the output limit")
	ErrInput     = errors.New("cannot read the encrypted backup; choose a regular, non-linked backup file within the input size limit")
	ErrDownload  = errors.New("cannot download the encrypted backup; check outbound access and backup availability, then retry")
	ErrIntegrity = errors.New("backup integrity verification failed; the file may be incomplete, modified, or encrypted with a different key; no restore output was published")
	ErrFormat    = errors.New("this backup format is not supported by the installed agent; update db-agent before retrying; no restore output was published")
	ErrCleanup   = errors.New("cannot remove private temporary restore output; ask a local administrator to inspect .db-agent-restore-* files in the output directory")
)

type safeError struct {
	message error
	cause   error
}

func (e *safeError) Error() string        { return e.message.Error() }
func (e *safeError) Unwrap() error        { return e.cause }
func (e *safeError) Is(target error) bool { return target == e.message }

type Lookup interface {
	DownloadBackup(context.Context, string) (controlplane.DownloadBackupResponse, error)
}

type Options struct {
	Timeout        time.Duration
	MaxInputBytes  int64
	MaxOutputBytes int64
	ReserveBytes   uint64
	Transport      http.RoundTripper
}

type Request struct {
	BackupID string
	File     string
	Output   string
	Key      key.Key
}

type Result struct {
	Artifact    artifact.Metadata
	OutputBytes int64
}

type Restorer struct {
	lookup    Lookup
	options   Options
	http      *http.Client
	freeSpace func(string) (uint64, error)
	wait      func(context.Context, time.Duration) error
}

func New(lookup Lookup, options Options) (*Restorer, error) {
	if options.Timeout < 0 || options.MaxInputBytes < 0 || options.MaxInputBytes == 1<<63-1 || options.MaxOutputBytes < 0 {
		return nil, errors.New("restore timeout and size limits must be positive")
	}
	if options.Timeout == 0 {
		options.Timeout = DefaultTimeout
	}
	if options.MaxInputBytes == 0 {
		options.MaxInputBytes = DefaultMaxInputBytes
	}
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if options.ReserveBytes == 0 {
		options.ReserveBytes = DefaultReserveBytes
	}
	return &Restorer{
		lookup: lookup, options: options, freeSpace: freeSpace, wait: wait,
		http: &http.Client{
			Transport: options.Transport,
			Timeout:   30 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (r *Restorer) Restore(parent context.Context, request Request) (Result, error) {
	if (request.BackupID == "") == (request.File == "") || request.Output == "" {
		return Result{}, errors.New("choose exactly one backup ID or encrypted file, and an output file")
	}
	if request.Key.Bytes() == [32]byte{} {
		return Result{}, errors.New("the original local recovery key is required; no replacement key was generated")
	}
	ctx, cancel := context.WithTimeout(parent, r.options.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	output, err := filepath.Abs(request.Output)
	if err != nil {
		return Result{}, &safeError{ErrOutput, err}
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return Result{}, ErrExists
		}
		return Result{}, &safeError{ErrOutput, err}
	}
	if request.File != "" {
		file, err := securefile.OpenRegular(request.File, r.options.MaxInputBytes)
		if err != nil {
			return Result{}, &safeError{ErrInput, err}
		}
		defer file.Close()
		return r.decode(ctx, output, file, request.Key, nil)
	}
	if r.lookup == nil {
		return Result{}, errors.New("remote restore requires an enrolled agent; offline recovery can use an encrypted file and saved key file")
	}
	return r.download(ctx, request.BackupID, output, request.Key)
}

func (r *Restorer) decode(ctx context.Context, output string, input io.Reader, k key.Key, expected *controlplane.DownloadBackupResponse) (result Result, err error) {
	staged, err := r.stage(output)
	if err != nil {
		return Result{}, err
	}
	defer func() { err = errors.Join(err, staged.close()) }()

	writer := &outputWriter{ctx: ctx, file: staged.file, dir: filepath.Dir(output), limit: r.options.MaxOutputBytes, reserve: r.options.ReserveBytes, freeSpace: r.freeSpace}
	limit := r.options.MaxInputBytes
	if expected != nil {
		limit = expected.SizeBytes
	}
	metadata, decodeErr := artifact.Decrypt(ctx, writer, io.LimitReader(input, limit+1), k)
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if writer.err != nil {
		return Result{}, writer.err
	}
	if decodeErr != nil {
		if errors.Is(decodeErr, artifact.ErrUnsupportedVersion) || errors.Is(decodeErr, artifact.ErrUnsupportedFormat) {
			return Result{}, &safeError{ErrFormat, decodeErr}
		}
		return Result{}, &safeError{ErrIntegrity, decodeErr}
	}
	if metadata.SizeBytes > r.options.MaxInputBytes || expected != nil && (metadata.SizeBytes != expected.SizeBytes || metadata.Checksum != expected.Checksum) {
		return Result{}, ErrIntegrity
	}
	if err := staged.file.Sync(); err != nil {
		return Result{}, &safeError{ErrOutput, err}
	}
	closeErr := staged.file.Close()
	staged.file = nil
	if closeErr != nil {
		return Result{}, &safeError{ErrOutput, closeErr}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := securefile.Publish(staged.path, output); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Result{}, ErrExists
		}
		return Result{}, &safeError{errors.New("cannot publish or sync restore output; a fully verified output may already exist; inspect it before retrying"), err}
	}
	return Result{Artifact: metadata, OutputBytes: writer.written}, nil
}
