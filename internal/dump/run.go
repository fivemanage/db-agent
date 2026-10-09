package dump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
)

const DefaultBytesPerSecond = 4 << 20

type Options struct {
	Timeout        time.Duration
	ThrottleIO     bool
	BytesPerSecond int64
	TempDir        string
}

// Run streams only the selected databases using a compatible tool returned by
// database.DetectTools. The caller must discard output on any error, including
// cleanup errors, and must make a blocking destination write cancelable. Run
// closes its own pipes and joins/reaps its child but never closes dst.
func Run(ctx context.Context, dst io.Writer, settings config.DatabaseConfig, tool database.Tool, options Options) (err error) {
	if err := validate(settings, tool, options); err != nil {
		return err
	}
	if options.Timeout == 0 {
		options.Timeout = config.DefaultDumpTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	path, dir, err := createOptions(settings, options.TempDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cleanupOptions(path, dir)) }()
	return execute(ctx, dst, buildCommand(settings, tool, path, dir), options)
}

func execute(ctx context.Context, dst io.Writer, cmd command, options Options) error {
	stdout, stdoutWrite, err := os.Pipe()
	if err != nil {
		return ErrStart
	}
	defer stdout.Close()
	defer stdoutWrite.Close()
	stderr, stderrWrite, err := os.Pipe()
	if err != nil {
		return ErrStart
	}
	defer stderr.Close()
	defer stderrWrite.Close()
	p, err := startProcess(cmd, stdoutWrite, stderrWrite)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return ErrPermission
		}
		return ErrStart
	}
	defer p.close()
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()

	var diagnostic stderrBuffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&diagnostic, stderr)
		close(stderrDone)
	}()
	waitDone := make(chan processResult, 1)
	go func() {
		result := p.wait()
		// A client that exits while a descendant holds stdout must not hang the run.
		result.cleanup = p.kill()
		waitDone <- result
	}()
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = p.kill()
			_ = stdout.Close()
			_ = stderr.Close()
		case <-stopWatch:
		}
	}()

	rate := int64(0)
	if options.ThrottleIO {
		rate = options.BytesPerSecond
		if rate == 0 {
			rate = DefaultBytesPerSecond
		}
	}
	copyErr := copyOutput(ctx, dst, stdout, rate)
	if copyErr != nil {
		_ = p.kill()
		_ = stdout.Close()
		_ = stderr.Close()
	}
	result := <-waitDone
	<-stderrDone
	close(stopWatch)
	<-watchDone
	defer clear(diagnostic.data[:])
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	if result.cleanup != nil {
		return ErrProcessCleanup
	}
	if copyErr != nil {
		return copyErr
	}
	if result.err != nil || result.code != 0 {
		return explainStderr(diagnostic.data[:diagnostic.used])
	}
	return nil
}

type processResult struct {
	code    int
	err     error
	cleanup error
}

type stderrBuffer struct {
	data [8192]byte
	used int
}

func (b *stderrBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.used += copy(b.data[b.used:], p)
	return n, nil
}

func contextFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("dump timed out; review backup.dump_timeout and database load: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("dump canceled; partial output must be discarded: %w", context.Canceled)
}
