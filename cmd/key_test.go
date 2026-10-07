package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func enrolledKeyConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveEnrollment(path, "https://api.example.com", "dac_existing"); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecoveryKeySetupDisplaysOnceThenNeverAgain(t *testing.T) {
	path := enrolledKeyConfig(t)
	var displayed string
	err := setupRecoveryKey(context.Background(), path, func(encoded string) error {
		settings, err := config.LoadDiagnostic(path)
		if err != nil || settings.Encryption.KeyFile == "" || settings.Encryption.KeyID == "" || settings.Encryption.KeyAcknowledged {
			t.Fatal("display preceded durable unacknowledged key binding")
		}
		displayed = encoded
		return nil
	}, func(existing bool, k key.Key) error {
		if existing || !k.Matches(displayed) {
			t.Fatal("new key could not be confirmed from saved copy")
		}
		return nil
	})
	if err != nil || displayed == "" {
		t.Fatalf("setup failed: %v", err)
	}
	settings, err := config.LoadDiagnostic(path)
	if err != nil || !settings.Encryption.KeyAcknowledged || settings.AgentCredential != "dac_existing" {
		t.Fatal("acknowledgement or enrollment lost")
	}
	before, err := os.ReadFile(settings.Encryption.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := setupRecoveryKey(context.Background(), path, func(string) error {
		t.Fatal("existing key redisplayed")
		return nil
	}, func(bool, key.Key) error {
		t.Fatal("acknowledged key prompted again")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(settings.Encryption.KeyFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing key changed on resume")
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(displayed)) {
		t.Fatal("recovery key persisted in config instead of its key file")
	}
}

func TestInterruptedDisplayAndConfirmationPreserveKeyWithoutRedisplay(t *testing.T) {
	for _, failDisplay := range []bool{true, false} {
		t.Run(map[bool]string{true: "display", false: "confirmation"}[failDisplay], func(t *testing.T) {
			path := enrolledKeyConfig(t)
			var saved string
			err := setupRecoveryKey(context.Background(), path, func(encoded string) error {
				saved = encoded
				if failDisplay {
					return errors.New("private output failure: " + encoded)
				}
				return nil
			}, func(bool, key.Key) error { return errors.New("private confirmation failure: " + saved) })
			if err == nil || strings.Contains(err.Error(), saved) {
				t.Fatal("interrupted setup accepted or secret echoed")
			}
			settings, err := config.LoadDiagnostic(path)
			if err != nil || settings.Encryption.KeyAcknowledged {
				t.Fatal("interrupted setup falsely acknowledged key")
			}
			before, err := os.ReadFile(settings.Encryption.KeyFile)
			if err != nil {
				t.Fatal(err)
			}
			err = setupRecoveryKey(context.Background(), path, func(string) error {
				t.Fatal("interrupted key redisplayed")
				return nil
			}, func(existing bool, k key.Key) error {
				if !existing || !k.Matches(saved) {
					t.Fatal("retry did not use original recovery key")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(settings.Encryption.KeyFile)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("retry replaced existing key")
			}
		})
	}
}

func TestOrphanedKeyAfterCrashBeforeBindingIsRecoveredWithoutDisplay(t *testing.T) {
	path := enrolledKeyConfig(t)
	keyPath, err := config.ResolveKeyPath(path, "")
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := key.Ensure(keyPath, true)
	if err != nil {
		t.Fatal(err)
	}
	err = setupRecoveryKey(context.Background(), path, func(string) error {
		t.Fatal("orphaned key redisplayed")
		return nil
	}, func(existing bool, k key.Key) error {
		if !existing || k.Bytes() != original.Bytes() {
			t.Fatal("orphaned key replaced")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetupRefusesKeyReplacementAndMissingConfiguredKey(t *testing.T) {
	path := enrolledKeyConfig(t)
	err := setupRecoveryKey(context.Background(), path, func(string) error { return nil }, func(bool, key.Key) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadDiagnostic(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(settings.Encryption.KeyFile); err != nil {
		t.Fatal(err)
	}
	show := func(string) error { t.Fatal("replacement key displayed"); return nil }
	confirm := func(bool, key.Key) error { t.Fatal("replacement key confirmed"); return nil }
	if err := setupRecoveryKey(context.Background(), path, show, confirm); err == nil {
		t.Fatal("missing configured key regenerated")
	}
	if _, err := os.Lstat(settings.Encryption.KeyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement key created")
	}
	if _, _, err := key.Ensure(settings.Encryption.KeyFile, true); err != nil {
		t.Fatal(err)
	}
	if err := setupRecoveryKey(context.Background(), path, show, confirm); err == nil {
		t.Fatal("changed key silently re-acknowledged")
	}
}

func TestCanceledKeySetupDoesNotCreateOrAcknowledgeKey(t *testing.T) {
	path := enrolledKeyConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := setupRecoveryKey(ctx, path, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled setup: %v", err)
	}
	keyPath, _ := config.ResolveKeyPath(path, "")
	if _, err := os.Lstat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled setup wrote a key")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	err := setupRecoveryKey(ctx, path, func(string) error { return nil }, func(bool, key.Key) error {
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	settings, err := config.LoadDiagnostic(path)
	if err != nil || settings.Encryption.KeyAcknowledged {
		t.Fatal("cancellation acknowledged key")
	}
}

func TestRedirectedKeySetupRefusesToPrintOrGenerateSecrets(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		t.Skip("requires redirected test input/output")
	}
	path := enrolledKeyConfig(t)
	command := &cobra.Command{}
	command.SetContext(context.Background())
	var output bytes.Buffer
	command.SetOut(&output)
	err := configureRecoveryKey(command, path)
	if err == nil || output.Len() != 0 || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatal("redirected recovery-key setup printed or accepted secrets")
	}
	keyPath, _ := config.ResolveKeyPath(path, "")
	if _, err := os.Lstat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("redirected setup generated a key before refusing display")
	}
}
