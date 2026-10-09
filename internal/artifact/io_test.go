package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/fivemanage/db-agent/internal/key"
)

var errTestIO = errors.New("test I/O failure")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errTestIO }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errTestIO }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

type failAfterWriter struct {
	dst       io.Writer
	remaining int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		n, _ := w.dst.Write(p[:w.remaining])
		w.remaining = 0
		return n, errTestIO
	}
	n, err := w.dst.Write(p)
	w.remaining -= n
	return n, err
}

type cancelReader struct {
	src    io.Reader
	cancel context.CancelFunc
}

func (r cancelReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.cancel()
	return n, err
}

func TestStreamIOFailures(t *testing.T) {
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			encoded, _ := encrypted(t, compression, bytes.Repeat([]byte("SQL;\n"), 1000))
			k := testKey(t, 0)
			tests := []struct {
				name string
				run  func() (Metadata, error)
				want error
			}{
				{"dump read", func() (Metadata, error) {
					return Encrypt(context.Background(), io.Discard, failingReader{}, k, compression)
				}, errTestIO},
				{"ciphertext write", func() (Metadata, error) {
					return Encrypt(context.Background(), failingWriter{}, bytes.NewReader(nil), k, compression)
				}, errTestIO},
				{"ciphertext short write", func() (Metadata, error) {
					return Encrypt(context.Background(), shortWriter{}, bytes.NewReader(nil), k, compression)
				}, io.ErrShortWrite},
				{"ciphertext read", func() (Metadata, error) {
					return Decrypt(context.Background(), io.Discard, io.MultiReader(bytes.NewReader(encoded[:headerSize+4]), failingReader{}), k)
				}, errTestIO},
				{"plaintext write", func() (Metadata, error) {
					return Decrypt(context.Background(), failingWriter{}, bytes.NewReader(encoded), k)
				}, errTestIO},
				{"plaintext short write", func() (Metadata, error) {
					return Decrypt(context.Background(), shortWriter{}, bytes.NewReader(encoded), k)
				}, io.ErrShortWrite},
				{"salt RNG", func() (Metadata, error) {
					return encrypt(context.Background(), io.Discard, bytes.NewReader(nil), k, compression, failingReader{})
				}, errTestIO},
				{"zero encryption key", func() (Metadata, error) {
					return Encrypt(context.Background(), io.Discard, bytes.NewReader(nil), key.Key{}, compression)
				}, ErrInvalidKey},
				{"zero decryption key", func() (Metadata, error) {
					return Decrypt(context.Background(), io.Discard, bytes.NewReader(encoded), key.Key{})
				}, ErrInvalidKey},
				{"invalid compression", func() (Metadata, error) {
					return Encrypt(context.Background(), io.Discard, bytes.NewReader(nil), k, "zip")
				}, ErrUnsupportedFormat},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					metadata, err := tt.run()
					if !errors.Is(err, tt.want) || metadata != (Metadata{}) {
						t.Fatalf("metadata=%+v err=%v, want %v", metadata, err, tt.want)
					}
				})
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			encoded, _ := encrypted(t, compression, []byte("SELECT 1;\n"))
			k := testKey(t, 0)
			for _, before := range []bool{true, false} {
				ctx, cancel := context.WithCancel(context.Background())
				if before {
					cancel()
				}
				metadata, err := Encrypt(ctx, io.Discard, cancelReader{bytes.NewReader(make([]byte, chunkSize*2)), cancel}, k, compression)
				cancel()
				if !errors.Is(err, context.Canceled) || metadata != (Metadata{}) {
					t.Fatalf("encrypt canceled (before=%t): %v", before, err)
				}
				ctx, cancel = context.WithCancel(context.Background())
				if before {
					cancel()
				}
				metadata, err = Decrypt(ctx, io.Discard, cancelReader{bytes.NewReader(encoded), cancel}, k)
				cancel()
				if !errors.Is(err, context.Canceled) || metadata != (Metadata{}) {
					t.Fatalf("decrypt canceled (before=%t): %v", before, err)
				}
			}
		})
	}
}

func TestPartialEncryptionIsNeverComplete(t *testing.T) {
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			k := testKey(t, 0)
			input := bytes.Repeat([]byte("INSERT INTO players VALUES (1);\n"), 100)
			var partial bytes.Buffer
			src := io.MultiReader(bytes.NewReader(input), failingReader{})
			metadata, err := Encrypt(context.Background(), &partial, src, k, compression)
			if !errors.Is(err, errTestIO) || metadata != (Metadata{}) {
				t.Fatalf("source failure: metadata=%+v err=%v", metadata, err)
			}
			if _, err := Decrypt(context.Background(), io.Discard, bytes.NewReader(partial.Bytes()), k); !errors.Is(err, ErrTruncated) {
				t.Fatalf("failed source produced complete artifact: %v", err)
			}

			complete, _ := encrypted(t, compression, input)
			for _, limit := range []int{headerSize + 1, headerSize + 5, len(complete) - 17, len(complete) - 1} {
				partial.Reset()
				dst := &failAfterWriter{dst: &partial, remaining: limit}
				metadata, err := Encrypt(context.Background(), dst, bytes.NewReader(input), k, compression)
				if !errors.Is(err, errTestIO) || metadata != (Metadata{}) {
					t.Fatalf("destination failure after %d: metadata=%+v err=%v", limit, metadata, err)
				}
				if _, err := Decrypt(context.Background(), io.Discard, bytes.NewReader(partial.Bytes()), k); !errors.Is(err, ErrTruncated) {
					t.Fatalf("failed destination produced complete artifact: %v", err)
				}
			}
		})
	}
}
