// Package artifact streams locally encrypted backup artifacts without temporary files.
package artifact

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/fivemanage/db-agent/internal/key"
)

type Compression string

const (
	Zstd Compression = "zstd"
	Gzip Compression = "gzip"

	version    = 1
	headerSize = 44
	chunkSize  = 64 << 10
	windowSize = 8 << 20
	maxRecords = uint64(1<<32 - 1) // Reserve the last nonce for the terminal record.
	keyContext = "db-agent artifact v1 AES-256-GCM"
	magic      = "DBAGBAK\x00"
)

var (
	ErrInvalidFormat      = errors.New("invalid backup artifact")
	ErrUnsupportedVersion = errors.New("unsupported backup artifact version")
	ErrUnsupportedFormat  = errors.New("unsupported backup compression or encryption format")
	ErrAuthentication     = errors.New("backup authentication failed; the recovery key is wrong or the artifact was modified")
	ErrTruncated          = errors.New("backup artifact is truncated")
	ErrCorrupt            = errors.New("backup artifact is corrupt")
	ErrTooLarge           = errors.New("backup exceeds the artifact record limit")
	ErrInvalidKey         = errors.New("a nonzero recovery key is required")
)

type header [headerSize]byte

func newHeader(compression Compression, random io.Reader) (header, error) {
	var h header
	copy(h[:8], magic)
	h[8], h[10] = version, 1
	switch compression {
	case Zstd:
		h[9] = 1
	case Gzip:
		h[9] = 2
	default:
		return header{}, ErrUnsupportedFormat
	}
	if _, err := io.ReadFull(random, h[12:]); err != nil {
		return header{}, fmt.Errorf("generate artifact salt: %w", err)
	}
	return h, nil
}

func readHeader(src io.Reader) (header, Compression, error) {
	var h header
	if err := readFull(src, h[:]); err != nil {
		return header{}, "", fmt.Errorf("read artifact header: %w", err)
	}
	if string(h[:8]) != magic {
		return header{}, "", ErrInvalidFormat
	}
	if h[8] != version {
		return header{}, "", fmt.Errorf("%w: %d", ErrUnsupportedVersion, h[8])
	}
	if h[10] != 1 || h[11] != 0 {
		return header{}, "", ErrUnsupportedFormat
	}
	switch h[9] {
	case 1:
		return h, Zstd, nil
	case 2:
		return h, Gzip, nil
	default:
		return header{}, "", ErrUnsupportedFormat
	}
}

func newAEAD(k key.Key, h header) (cipher.AEAD, error) {
	material := k.Bytes()
	defer clear(material[:])
	if material == [32]byte{} {
		return nil, ErrInvalidKey
	}
	derived, err := hkdf.Key(sha256.New, material[:], h[12:], keyContext, 32)
	if err != nil {
		return nil, fmt.Errorf("derive artifact key: %w", err)
	}
	defer clear(derived)
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("initialize artifact cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func recordNonce(index uint64) [12]byte {
	var nonce [12]byte
	binary.BigEndian.PutUint32(nonce[8:], uint32(index))
	return nonce
}

func recordAAD(h header, length [4]byte) [headerSize + 4]byte {
	var aad [headerSize + 4]byte
	copy(aad[:headerSize], h[:])
	copy(aad[headerSize:], length[:])
	return aad
}

func readFull(src io.Reader, p []byte) error {
	_, err := io.ReadFull(src, p)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	}
	return err
}

func writeFull(dst io.Writer, p []byte) error {
	n, err := dst.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return err
}
