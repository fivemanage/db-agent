package artifact

import (
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
)

type recordWriter struct {
	dst    io.Writer
	aead   cipher.AEAD
	header header
	index  uint64
	used   int
	err    error
	plain  [chunkSize]byte
	sealed [chunkSize + 16]byte
}

func (w *recordWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		n := copy(w.plain[w.used:], p)
		w.used += n
		written += n
		p = p[n:]
		if w.used == chunkSize {
			if err := w.emit(w.plain[:w.used]); err != nil {
				w.err = err
				return written, err
			}
			w.used = 0
		}
	}
	return written, nil
}

func (w *recordWriter) emit(plain []byte) error {
	if w.index > maxRecords || len(plain) > 0 && w.index == maxRecords {
		return ErrTooLarge
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(plain)))
	nonce, aad := recordNonce(w.index), recordAAD(w.header, length)
	sealed := w.aead.Seal(w.sealed[:0], nonce[:], plain, aad[:])
	if err := writeFull(w.dst, length[:]); err != nil {
		return fmt.Errorf("write artifact record length: %w", err)
	}
	if err := writeFull(w.dst, sealed); err != nil {
		return fmt.Errorf("write encrypted artifact record: %w", err)
	}
	w.index++
	return nil
}

func (w *recordWriter) finish() error {
	if w.err != nil {
		return w.err
	}
	if w.used > 0 {
		if err := w.emit(w.plain[:w.used]); err != nil {
			return err
		}
		w.used = 0
	}
	// An authenticated terminal record prevents a valid prefix being a valid backup.
	return w.emit(nil)
}

type recordReader struct {
	src      io.Reader
	aead     cipher.AEAD
	header   header
	index    uint64
	short    bool
	finished bool
	err      error
	pending  []byte
	sealed   [chunkSize + 16]byte
}

func (r *recordReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if len(r.pending) == 0 {
		if r.finished {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			r.err = err
			return 0, err
		}
		if r.finished {
			return 0, io.EOF
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *recordReader) next() error {
	if r.index > maxRecords {
		return ErrTooLarge
	}
	var length [4]byte
	if err := readFull(r.src, length[:]); err != nil {
		return fmt.Errorf("read artifact record length: %w", err)
	}
	size := binary.BigEndian.Uint32(length[:])
	if size > chunkSize || size > 0 && r.short {
		return fmt.Errorf("%w: invalid record length", ErrCorrupt)
	}
	if size > 0 && r.index == maxRecords {
		return ErrTooLarge
	}
	sealed := r.sealed[:int(size)+r.aead.Overhead()]
	if err := readFull(r.src, sealed); err != nil {
		return fmt.Errorf("read encrypted artifact record: %w", err)
	}
	nonce, aad := recordNonce(r.index), recordAAD(r.header, length)
	plain, err := r.aead.Open(sealed[:0], nonce[:], sealed, aad[:])
	if err != nil {
		return ErrAuthentication
	}
	r.index++
	if size == 0 {
		var trailing [1]byte
		if _, err := io.ReadFull(r.src, trailing[:]); err != io.EOF {
			if err != nil {
				return fmt.Errorf("read artifact end: %w", err)
			}
			return fmt.Errorf("%w: trailing data", ErrCorrupt)
		}
		r.finished = true
		return nil
	}
	r.short = size < chunkSize
	r.pending = plain
	return nil
}

func (r *recordReader) finish() error {
	if r.err != nil {
		return r.err
	}
	if len(r.pending) != 0 {
		return fmt.Errorf("%w: unconsumed compressed data", ErrCorrupt)
	}
	if !r.finished {
		if err := r.next(); err != nil {
			return err
		}
		if !r.finished {
			return fmt.Errorf("%w: unconsumed compressed record", ErrCorrupt)
		}
	}
	return nil
}
