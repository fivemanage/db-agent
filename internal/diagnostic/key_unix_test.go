//go:build !windows

package diagnostic

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestKeyRejectsBroadPermissionsAndFIFOs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("private-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if CheckKey(path).Passed {
		t.Fatal("world-readable key accepted")
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if CheckKey(fifo).Passed {
		t.Fatal("FIFO accepted")
	}
}
