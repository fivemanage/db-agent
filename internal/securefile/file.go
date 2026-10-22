package securefile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var ErrUnsafe = errors.New("file must be regular, private, and not a symbolic link; use owner-only permissions on Unix or System/Administrators-only ACLs on Windows")

// Create publishes only a complete, synced private file and never replaces an
// existing destination. A publication/sync failure may leave a valid destination;
// callers must preserve it rather than retrying with new secret material.
func Create(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create private directory: %w", err)
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate private staging name: %w", err)
	}

	staged := filepath.Join(filepath.Dir(path), ".db-agent-private-"+hex.EncodeToString(nonce[:]))
	file, err := OpenNew(staged)
	if err != nil {
		return fmt.Errorf("create private staging file: %w", err)
	}
	defer os.Remove(staged)
	defer file.Close()

	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write private file: %w", err)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync private file: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close private file: %w", err)
	}

	return publish(staged, path)
}

func Read(path string, maxBytes int64) ([]byte, error) {
	file, err := Open(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maxBytes {
		return nil, ErrUnsafe
	}

	return contents, nil
}

// Open validates a private regular file without reading its contents into memory.
func Open(path string, maxBytes int64) (*os.File, error) {
	file, err := OpenRegular(path, maxBytes)
	if err != nil {
		return nil, err
	}
	if err := checkPermissions(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// OpenRegular rejects links and special files but permits public ciphertext files.
// Secrets must use Open, which additionally validates private permissions.
func OpenRegular(path string, maxBytes int64) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, ErrUnsafe
	}

	file, err := openRead(path)
	if err != nil {
		return nil, err
	}

	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() > maxBytes {
		file.Close()
		return nil, ErrUnsafe
	}

	return file, nil
}

// Publish atomically publishes a complete, synced file without replacing any
// destination. Stage in the destination directory and retain private permissions.
// A directory-sync error may leave a complete destination; never delete it on error.
func Publish(from, to string) error {
	return publish(from, to)
}
