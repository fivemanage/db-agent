//go:build !windows

package restorejob

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestPublicPermissionsAndSymlinkStateAreRejected(t *testing.T) {
	for _, mode := range []string{"public", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			j := newJournal(t)
			s := openJob(t, j, testJob())
			s.Close()
			info, err := os.Stat(s.path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("state not private: %v", err)
			}
			if mode == "public" {
				err = os.Chmod(s.path, 0644)
			} else {
				target := s.path + ".original"
				if err := os.Rename(s.path, target); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(target, s.path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if next, err := j.Open(context.Background(), testJob()); next != nil || !errors.Is(err, ErrState) {
				t.Fatalf("unsafe state allowed restore: %v", err)
			}
		})
	}
}
