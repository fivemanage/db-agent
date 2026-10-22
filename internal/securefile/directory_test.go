package securefile

import (
	"os"
	"runtime"
	"testing"
)

func TestPrivateWorkspaceCanHoldOnlyExplicitlyProtectedOutput(t *testing.T) {
	dir, err := MkdirTemp(t.TempDir(), ".restore-work-")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatal("private workspace was not a directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
		t.Fatal("workspace permissions were public")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal("empty private workspace couldn't be cleaned up")
	}
}
