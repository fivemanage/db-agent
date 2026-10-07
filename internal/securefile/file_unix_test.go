//go:build !windows

package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadRefusesBroadPermissionsSymlinksAndFIFOs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private")
	if err := Create(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, 128); !errors.Is(err, ErrUnsafe) {
		t.Fatal("group-readable key accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link, 128); !errors.Is(err, ErrUnsafe) {
		t.Fatal("symbolic link accepted")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(fifo, 128); !errors.Is(err, ErrUnsafe) {
		t.Fatal("FIFO accepted")
	}
	if err := Create(link, []byte("replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatal("symbolic link destination replaced")
	}
	contents, err := Read(path, 128)
	if err != nil || string(contents) != "secret" {
		t.Fatal("symlink target changed")
	}
}
