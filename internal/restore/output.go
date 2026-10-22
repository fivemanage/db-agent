package restore

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/fivemanage/db-agent/internal/securefile"
)

type stagedOutput struct {
	file *os.File
	path string
}

func (r *Restorer) stage(output string) (*stagedOutput, error) {
	dir := filepath.Dir(output)
	available, err := r.freeSpace(dir)
	if err != nil {
		return nil, &safeError{ErrOutput, err}
	}
	if available < r.options.ReserveBytes {
		return nil, ErrLimit
	}
	path := filepath.Join(dir, ".db-agent-restore-"+rand.Text())
	file, err := securefile.OpenNew(path)
	if err != nil {
		return nil, &safeError{ErrOutput, err}
	}
	return &stagedOutput{file: file, path: path}, nil
}

func (s *stagedOutput) close() error {
	var closeErr error
	if s.file != nil {
		closeErr = s.file.Close()
	}
	removeErr := os.Remove(s.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if closeErr != nil || removeErr != nil {
		return &safeError{ErrCleanup, errors.Join(closeErr, removeErr)}
	}
	return nil
}

type outputWriter struct {
	ctx       context.Context
	file      io.Writer
	dir       string
	limit     int64
	reserve   uint64
	written   int64
	freeSpace func(string) (uint64, error)
	err       error
}

func (w *outputWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return 0, err
	}
	if int64(len(p)) > w.limit-w.written {
		w.err = ErrLimit
		return 0, w.err
	}
	available, err := w.freeSpace(w.dir)
	if err != nil {
		w.err = &safeError{ErrOutput, err}
		return 0, w.err
	}
	if available < w.reserve || uint64(len(p)) > available-w.reserve {
		w.err = ErrLimit
		return 0, w.err
	}
	n, err := w.file.Write(p)
	w.written += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = &safeError{ErrOutput, err}
	}
	return n, w.err
}
