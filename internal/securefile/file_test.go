package securefile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCreatePublishesPrivateFileWithoutReplacingExistingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "secret")
	contents := []byte("private secret material")
	if err := Create(path, contents); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(path, 128)
	if err != nil || !bytes.Equal(loaded, contents) {
		t.Fatalf("private read failed: %v", err)
	}
	if err := Create(path, []byte("replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive publication failed: %v", err)
	}
	loaded, err = Read(path, 128)
	if err != nil || !bytes.Equal(loaded, contents) {
		t.Fatal("existing private file changed")
	}
	if _, err := Read(path, 4); err == nil {
		t.Fatal("oversized private file accepted")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("private staging files leaked")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("private file mode is not 0600")
		}
	}
}

func TestReadRefusesDirectories(t *testing.T) {
	if _, err := Read(t.TempDir(), 128); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("directory accepted as a private file: %v", err)
	}
}
