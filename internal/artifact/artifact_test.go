package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/fivemanage/db-agent/internal/key"
)

func testKey(t testing.TB, seed byte) key.Key {
	t.Helper()
	var material [32]byte
	for i := range material {
		material[i] = seed + byte(i)
	}
	k, err := key.Parse(key.Prefix + base64.RawURLEncoding.EncodeToString(material[:]))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func encrypted(t testing.TB, compression Compression, plain []byte) ([]byte, Metadata) {
	t.Helper()
	var dst bytes.Buffer
	metadata, err := Encrypt(context.Background(), &dst, bytes.NewReader(plain), testKey(t, 0), compression)
	if err != nil {
		t.Fatal(err)
	}
	return dst.Bytes(), metadata
}

func assertMetadata(t testing.TB, metadata Metadata, encoded []byte, compression Compression) {
	t.Helper()
	digest := sha256.Sum256(encoded)
	want := Metadata{Version: 1, Compression: compression, SizeBytes: int64(len(encoded)), Checksum: fmt.Sprintf("sha256:%x", digest)}
	if metadata != want {
		t.Fatalf("metadata = %+v, want %+v", metadata, want)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, compression := range []Compression{Zstd, Gzip} {
		for name, input := range map[string][]byte{
			"empty": nil,
			"sql":   []byte("CREATE TABLE players (id INT);\nINSERT INTO players VALUES (1);\n"),
			"large": bytes.Repeat([]byte("INSERT INTO players VALUES (123456789);\n"), 300000),
		} {
			t.Run(string(compression)+"/"+name, func(t *testing.T) {
				encoded, metadata := encrypted(t, compression, input)
				assertMetadata(t, metadata, encoded, compression)
				var restored bytes.Buffer
				decoded, err := Decrypt(context.Background(), &restored, bytes.NewReader(encoded), testKey(t, 0))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(restored.Bytes(), input) || decoded != metadata {
					t.Fatal("restored input or metadata differs")
				}
			})
		}
	}
}

func TestDefaultAndFreshSalt(t *testing.T) {
	first, metadata := encrypted(t, "", []byte("SQL"))
	second, _ := encrypted(t, "", []byte("SQL"))
	assertMetadata(t, metadata, first, Zstd)
	if bytes.Equal(first[12:headerSize], second[12:headerSize]) || bytes.Equal(first, second) {
		t.Fatal("separate artifacts reused salt or ciphertext")
	}
}

func TestWrongKey(t *testing.T) {
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			encoded, _ := encrypted(t, compression, []byte("private SQL"))
			var restored bytes.Buffer
			metadata, err := Decrypt(context.Background(), &restored, bytes.NewReader(encoded), testKey(t, 1))
			if !errors.Is(err, ErrAuthentication) || metadata != (Metadata{}) || restored.Len() != 0 {
				t.Fatalf("wrong key: metadata=%+v output=%d err=%v", metadata, restored.Len(), err)
			}
		})
	}
}

func TestTruncationAtEveryByte(t *testing.T) {
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			encoded, _ := encrypted(t, compression, []byte("SELECT 1;\n"))
			for end := 0; end < len(encoded); end++ {
				metadata, err := Decrypt(context.Background(), io.Discard, bytes.NewReader(encoded[:end]), testKey(t, 0))
				if !errors.Is(err, ErrTruncated) || metadata != (Metadata{}) {
					t.Fatalf("cut at %d: metadata=%+v err=%v", end, metadata, err)
				}
			}
		})
	}
}

func TestTampering(t *testing.T) {
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			original, _ := encrypted(t, compression, []byte("private SQL"))
			firstSize := int(binary.BigEndian.Uint32(original[headerSize:]))
			tests := []struct {
				name   string
				mutate func([]byte) []byte
				want   error
			}{
				{"magic", flipByte(0), ErrInvalidFormat},
				{"version", flipByte(8), ErrUnsupportedVersion},
				{"encryption", flipByte(10), ErrUnsupportedFormat},
				{"reserved", flipByte(11), ErrUnsupportedFormat},
				{"salt", flipByte(12), ErrAuthentication},
				{"ciphertext", flipByte(headerSize + 4), ErrAuthentication},
				{"tag", flipByte(headerSize + 4 + firstSize), ErrAuthentication},
				{"terminal tag", flipByte(len(original) - 1), ErrAuthentication},
				{"trailing data", func(p []byte) []byte { return append(p, 0) }, ErrCorrupt},
				{"concatenated artifacts", func(p []byte) []byte { return append(p, original...) }, ErrCorrupt},
				{"oversized length", func(p []byte) []byte {
					binary.BigEndian.PutUint32(p[headerSize:], chunkSize+1)
					return p
				}, ErrCorrupt},
				{"shorter length", func(p []byte) []byte {
					binary.BigEndian.PutUint32(p[headerSize:], uint32(firstSize-1))
					return p
				}, ErrAuthentication},
				{"supported compression switch", func(p []byte) []byte {
					p[9] = 3 - p[9]
					return p
				}, ErrAuthentication},
				{"unknown compression", func(p []byte) []byte { p[9] = 42; return p }, ErrUnsupportedFormat},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					mutated := tt.mutate(bytes.Clone(original))
					metadata, err := Decrypt(context.Background(), io.Discard, bytes.NewReader(mutated), testKey(t, 0))
					if !errors.Is(err, tt.want) || metadata != (Metadata{}) {
						t.Fatalf("metadata=%+v err=%v, want %v", metadata, err, tt.want)
					}
				})
			}
		})
	}
}

func flipByte(index int) func([]byte) []byte {
	return func(p []byte) []byte { p[index] ^= 1; return p }
}

func TestRecordOrderAndBoundaries(t *testing.T) {
	input := make([]byte, chunkSize*4)
	_, _ = rand.New(rand.NewSource(1)).Read(input)
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			original, _ := encrypted(t, compression, input)
			var restored bytes.Buffer
			if _, err := Decrypt(context.Background(), &restored, bytes.NewReader(original), testKey(t, 0)); err != nil || !bytes.Equal(restored.Bytes(), input) {
				t.Fatalf("multi-record round trip: %v", err)
			}
			firstEnd := headerSize + 4 + chunkSize + 16
			secondEnd := firstEnd + 4 + chunkSize + 16
			for name, changed := range map[string][]byte{
				"remove first": append(bytes.Clone(original[:headerSize]), original[firstEnd:]...),
				"duplicate":    append(append(bytes.Clone(original[:firstEnd]), original[headerSize:firstEnd]...), original[firstEnd:]...),
				"swap":         append(append(append(bytes.Clone(original[:headerSize]), original[firstEnd:secondEnd]...), original[headerSize:firstEnd]...), original[secondEnd:]...),
				"valid prefix": bytes.Clone(original[:secondEnd]),
			} {
				t.Run(name, func(t *testing.T) {
					_, err := Decrypt(context.Background(), io.Discard, bytes.NewReader(changed), testKey(t, 0))
					want := ErrAuthentication
					if name == "valid prefix" {
						want = ErrTruncated
					}
					if !errors.Is(err, want) {
						t.Fatalf("err=%v, want %v", err, want)
					}
				})
			}
		})
	}
}

// This vector was produced independently with Node's HKDF, AES-GCM, and gzip.
const vectorHex = "4442414742414b0001020100a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf0000001e66187b1597126ae2659e39a5c5ec175379bddbae150231400f9b63d8dc9e4e45419596cacbabd31fac5530a495460000000015a71f15d1befdba4f1fa8d46da4aff1"

func TestIndependentVector(t *testing.T) {
	encoded, err := hex.DecodeString(vectorHex)
	if err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	metadata, err := Decrypt(context.Background(), &restored, bytes.NewReader(encoded), testKey(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if restored.String() != "SELECT 1;\n" {
		t.Fatal("independent vector plaintext differs")
	}
	assertMetadata(t, metadata, encoded, Gzip)
	if metadata.Checksum != "sha256:21b84b56ec2b6f07b328326f0881105771b24d07df16289a0b34d5ecb31446f7" {
		t.Fatal("independent vector checksum differs")
	}

	var h header
	copy(h[:], encoded[:headerSize])
	aead, err := newAEAD(testKey(t, 0), h)
	if err != nil {
		t.Fatal(err)
	}
	compressed, _ := hex.DecodeString("1f8b08000000000000030b76f571750e5130b4e602007fa90e500a000000")
	var generated bytes.Buffer
	generated.Write(h[:])
	records := &recordWriter{dst: &generated, aead: aead, header: h}
	if _, err := records.Write(compressed); err != nil {
		t.Fatal(err)
	}
	if err := records.finish(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated.Bytes(), encoded) {
		t.Fatal("record encoding differs from independent vector")
	}
}

func TestAuthenticatedCorruptCompression(t *testing.T) {
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			for _, compressed := range [][]byte{nil, []byte("not a compressed stream")} {
				encoded := wrapCompressed(t, compression, compressed)
				_, err := Decrypt(context.Background(), io.Discard, bytes.NewReader(encoded), testKey(t, 0))
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("err=%v, want corrupt compression", err)
				}
			}
		})
	}
	// A streaming zstd frame advertising a 16 MiB window must be rejected before allocation.
	encoded := wrapCompressed(t, Zstd, []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0x70, 1, 0, 0})
	if _, err := Decrypt(context.Background(), io.Discard, bytes.NewReader(encoded), testKey(t, 0)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized window: %v", err)
	}
}

func wrapCompressed(t testing.TB, compression Compression, compressed []byte) []byte {
	t.Helper()
	h, err := newHeader(compression, bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := newAEAD(testKey(t, 0), h)
	if err != nil {
		t.Fatal(err)
	}
	var dst bytes.Buffer
	dst.Write(h[:])
	records := &recordWriter{dst: &dst, header: h, aead: aead}
	if _, err := records.Write(compressed); err != nil {
		t.Fatal(err)
	}
	if err := records.finish(); err != nil {
		t.Fatal(err)
	}
	return dst.Bytes()
}

func TestStreamingLargeInput(t *testing.T) {
	const size = 128 << 20
	for _, compression := range []Compression{Zstd, Gzip} {
		t.Run(string(compression), func(t *testing.T) {
			// Neither side retains the dump or ciphertext; the pipe provides backpressure.
			reader, writer := io.Pipe()
			defer reader.Close()
			type result struct {
				metadata Metadata
				err      error
			}
			done := make(chan result, 1)
			originalHash := sha256.New()
			k := testKey(t, 0)
			go func() {
				defer writer.Close()
				src := io.TeeReader(io.LimitReader(rand.New(rand.NewSource(42)), size), originalHash)
				metadata, err := Encrypt(context.Background(), writer, src, k, compression)
				_ = writer.CloseWithError(err)
				done <- result{metadata, err}
			}()
			restoredHash := sha256.New()
			decoded, err := Decrypt(context.Background(), restoredHash, reader, k)
			_ = reader.CloseWithError(err)
			encoded := <-done
			if err != nil || encoded.err != nil {
				t.Fatalf("encrypt=%v decrypt=%v", encoded.err, err)
			}
			if decoded != encoded.metadata || !bytes.Equal(originalHash.Sum(nil), restoredHash.Sum(nil)) {
				t.Fatal("streamed input or metadata differs")
			}
			if decoded.SizeBytes < size {
				t.Fatal("incompressible input did not exercise many records")
			}
		})
	}
}

func TestRecordLimit(t *testing.T) {
	h, _ := newHeader(Gzip, bytes.NewReader(make([]byte, 32)))
	aead, _ := newAEAD(testKey(t, 0), h)
	var dst bytes.Buffer
	w := &recordWriter{dst: &dst, header: h, aead: aead, index: maxRecords}
	if err := w.emit([]byte{1}); !errors.Is(err, ErrTooLarge) || dst.Len() != 0 {
		t.Fatalf("last nonce used for data: %v", err)
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	r := &recordReader{src: bytes.NewReader(dst.Bytes()), header: h, aead: aead, index: maxRecords}
	if err := r.finish(); err != nil {
		t.Fatal(err)
	}
	if err := w.finish(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("nonce overflow: %v", err)
	}
}

func FuzzDecrypt(f *testing.F) {
	k := testKey(f, 0)
	vector, _ := hex.DecodeString(vectorHex)
	f.Add(vector)
	f.Add([]byte("not a backup"))
	for _, compression := range []Compression{Zstd, Gzip} {
		encoded, _ := encrypted(f, compression, []byte("SELECT 1;\n"))
		f.Add(encoded)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 1<<20 {
			t.Skip()
		}
		metadata, err := Decrypt(context.Background(), io.Discard, bytes.NewReader(input), k)
		if err != nil {
			if metadata != (Metadata{}) {
				t.Fatal("failed artifact returned success metadata")
			}
			return
		}
		assertMetadata(t, metadata, input, metadata.Compression)
	})
}

func BenchmarkEncrypt(b *testing.B) {
	for _, compression := range []Compression{Zstd, Gzip} {
		for _, size := range []int64{1 << 20, 128 << 20} {
			b.Run(fmt.Sprintf("%s/%dMiB", compression, size>>20), func(b *testing.B) {
				k := testKey(b, 0)
				b.SetBytes(size)
				b.ReportAllocs()
				for b.Loop() {
					src := io.LimitReader(rand.New(rand.NewSource(42)), size)
					if _, err := Encrypt(context.Background(), io.Discard, src, k, compression); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
