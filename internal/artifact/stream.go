package artifact

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/fivemanage/db-agent/internal/key"
)

// Metadata describes the entire encrypted artifact, including its header and tags.
// It is returned only after the stream has completed successfully.
type Metadata struct {
	Version     int
	Compression Compression
	SizeBytes   int64
	Checksum    string
}

// Encrypt defaults to zstd when compression is empty. Neither stream is closed.
// On failure, the caller must discard any partial ciphertext written to dst.
// Blocking I/O must also be made cancelable by the caller (e.g. closing a pipe).
func Encrypt(ctx context.Context, dst io.Writer, src io.Reader, k key.Key, compression Compression) (Metadata, error) {
	return encrypt(ctx, dst, src, k, compression, rand.Reader)
}

func encrypt(ctx context.Context, dst io.Writer, src io.Reader, k key.Key, compression Compression, random io.Reader) (Metadata, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if compression == "" {
		compression = Zstd
	}
	h, err := newHeader(compression, random)
	if err != nil {
		return Metadata{}, err
	}
	aead, err := newAEAD(k, h)
	if err != nil {
		return Metadata{}, err
	}
	meter := &meter{digest: sha256.New()}
	out := measuredWriter{contextWriter{ctx, dst}, meter}
	if err := writeFull(out, h[:]); err != nil {
		return Metadata{}, fmt.Errorf("write artifact header: %w", err)
	}
	records := &recordWriter{dst: out, aead: aead, header: h}
	defer clear(records.plain[:])
	var compressor io.WriteCloser
	if compression == Zstd {
		compressor, err = zstd.NewWriter(records, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(windowSize), zstd.WithEncoderCRC(true), zstd.WithZeroFrames(true))
	} else {
		compressor = gzip.NewWriter(records)
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("initialize backup compression: %w", err)
	}
	buffer := make([]byte, 32<<10)
	defer clear(buffer)
	if _, err := io.CopyBuffer(compressor, contextReader{ctx, src}, buffer); err != nil {
		// Close releases compressor resources, but never finalize a failed artifact.
		_ = compressor.Close()
		return Metadata{}, fmt.Errorf("compress backup: %w", err)
	}
	if err := compressor.Close(); err != nil {
		return Metadata{}, fmt.Errorf("finish backup compression: %w", err)
	}
	if err := records.finish(); err != nil {
		return Metadata{}, err
	}
	return meter.metadata(compression), nil
}

// Decrypt authenticates each compressed record before decompression. A later
// failure may follow partial plaintext output: restore must stage privately,
// discard on error, and publish only after this function succeeds. Neither
// stream is closed; callers must arrange cancellation of blocking I/O.
func Decrypt(ctx context.Context, dst io.Writer, src io.Reader, k key.Key) (Metadata, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	meter := &meter{digest: sha256.New()}
	in := measuredReader{contextReader{ctx, src}, meter}
	h, compression, err := readHeader(in)
	if err != nil {
		return Metadata{}, err
	}
	aead, err := newAEAD(k, h)
	if err != nil {
		return Metadata{}, err
	}
	records := &recordReader{src: in, aead: aead, header: h}
	defer clear(records.sealed[:])
	var decompressor io.Reader
	if compression == Zstd {
		decoder, decodeErr := zstd.NewReader(records, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(windowSize), zstd.WithDecoderMaxMemory(windowSize))
		if decodeErr != nil {
			return Metadata{}, fmt.Errorf("initialize backup decompression: %w", decodeErr)
		}
		defer decoder.Close()
		decompressor = decoder
	} else {
		decoder, decodeErr := gzip.NewReader(records)
		if decodeErr != nil {
			return Metadata{}, decompressionError(records, decodeErr)
		}
		defer decoder.Close()
		decompressor = decoder
	}
	out := &restoreWriter{dst: contextWriter{ctx, dst}}
	buffer := make([]byte, 32<<10)
	defer clear(buffer)
	if _, err := io.CopyBuffer(out, decompressor, buffer); err != nil {
		if out.err != nil {
			return Metadata{}, fmt.Errorf("write restored backup: %w", out.err)
		}
		return Metadata{}, decompressionError(records, err)
	}
	if err := records.finish(); err != nil {
		return Metadata{}, err
	}
	if records.index < 2 {
		return Metadata{}, fmt.Errorf("%w: missing compressed stream", ErrCorrupt)
	}
	return meter.metadata(compression), nil
}

func decompressionError(records *recordReader, err error) error {
	// Codec read-ahead must not hide a more useful authentication/transport failure.
	if records.err != nil {
		return records.err
	}
	return fmt.Errorf("decompress backup: %w: %w", ErrCorrupt, err)
}

type restoreWriter struct {
	dst io.Writer
	err error
}

func (w *restoreWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.err = err
	return n, err
}

type meter struct {
	digest hash.Hash
	size   int64
}

func (m *meter) add(p []byte) {
	_, _ = m.digest.Write(p)
	m.size += int64(len(p))
}

func (m *meter) metadata(compression Compression) Metadata {
	return Metadata{Version: version, Compression: compression, SizeBytes: m.size, Checksum: "sha256:" + hex.EncodeToString(m.digest.Sum(nil))}
}

type measuredWriter struct {
	dst io.Writer
	m   *meter
}

func (w measuredWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.m.add(p[:n])
	return n, err
}

type measuredReader struct {
	src io.Reader
	m   *meter
}

func (r measuredReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.m.add(p[:n])
	return n, err
}

type contextReader struct {
	ctx context.Context
	src io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}

type contextWriter struct {
	ctx context.Context
	dst io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.dst.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
