package key

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fivemanage/db-agent/internal/securefile"
)

func TestRecoveryFormatAndRedactedFormatting(t *testing.T) {
	k, err := generate(bytes.NewReader(bytes.Repeat([]byte{0x37}, 32)))
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := Parse(k.RecoveryString() + "\n")
	if err != nil || parsed.Bytes() != k.Bytes() || !k.Matches(k.RecoveryString()) || len(k.ID()) != 64 {
		t.Fatal("recovery format did not round-trip")
	}

	for _, formatted := range []string{fmt.Sprint(k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k), fmt.Sprintf("%s", k), fmt.Sprintf("%x", k), fmt.Sprintf("%d", k)} {
		if strings.Contains(formatted, k.RecoveryString()) || !strings.Contains(formatted, "redacted") {
			t.Fatal("ordinary formatting disclosed a recovery key")
		}
	}

	for _, input := range []string{"private-secret", "dbak1_private-secret", Prefix + strings.Repeat("!", 43), Prefix + strings.Repeat("A", 42) + "B"} {
		_, err := Parse(input)
		if err == nil || strings.Contains(err.Error(), input) {
			t.Fatal("invalid recovery key accepted or echoed")
		}
	}

	if k.Matches("not-a-key") {
		t.Fatal("invalid confirmation matched")
	}
}

func TestEnsurePreservesExistingKeyAndNeverReplacesMissingConfiguredKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encryption.key")
	first, created, err := Ensure(path, true)
	if err != nil || !created {
		t.Fatalf("key creation: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, allowCreate := range []bool{true, false} {
		current, created, err := Ensure(path, allowCreate)
		if err != nil || created || current.Bytes() != first.Bytes() {
			t.Fatal("existing key rotated")
		}
	}

	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, contents) {
		t.Fatal("existing key file changed")
	}

	missing := filepath.Join(t.TempDir(), "missing.key")
	if _, _, err := Ensure(missing, false); err == nil {
		t.Fatal("configured missing key regenerated")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement material written")
	}
}

func TestEnsureRefusesMalformedExistingKeyWithoutDisclosingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encryption.key")
	const secret = "existing-invalid-private-key"
	if err := securefile.Create(path, []byte(secret)); err != nil {
		t.Fatal(err)
	}

	_, created, err := Ensure(path, true)
	if err == nil || created || strings.Contains(err.Error(), secret) {
		t.Fatal("invalid existing key overwritten or disclosed")
	}

	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != secret {
		t.Fatal("invalid key file was removed or replaced")
	}

	if _, err := generate(bytes.NewReader([]byte("short"))); err == nil {
		t.Fatal("short RNG output accepted")
	}
}

func TestConcurrentCreationPublishesOneCompleteKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encryption.key")
	var creators atomic.Int32
	var group sync.WaitGroup
	ids := make(chan string, 12)

	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			k, created, err := Ensure(path, true)
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				creators.Add(1)
			}
			ids <- k.ID()
		}()
	}

	group.Wait()
	close(ids)

	k, err := Load(path)
	if err != nil || creators.Load() != 1 {
		t.Fatalf("key publication: creators=%d err=%v", creators.Load(), err)
	}

	for id := range ids {
		if id != k.ID() {
			t.Fatal("concurrent callers received different keys")
		}
	}
}
