package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func configureRecoveryKey(cmd *cobra.Command, path string) error {
	settings, err := config.LoadDiagnostic(path)
	if err != nil {
		return err
	}

	if settings.Encryption.KeyAcknowledged {
		keyPath, err := config.ResolveKeyPath(path, settings.Encryption.KeyFile)
		if err != nil {
			return err
		}

		k, _, err := key.Ensure(keyPath, false)
		if err != nil {
			return err
		}
		if k.ID() != settings.Encryption.KeyID {
			return errors.New("recovery key does not match the acknowledged fingerprint; restore the original key; no key was changed")
		}

		if err := config.SaveKeyBinding(path, keyPath, k.ID(), true); err != nil {
			return err
		}

		fmt.Fprintln(cmd.OutOrStdout(), "Existing acknowledged recovery key preserved; it will not be displayed again.")

		return nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("recovery-key setup requires an interactive terminal with unredirected stdout; do not record the terminal or run under a service/log collector")
	}

	input := bufio.NewReader(os.Stdin)
	err = setupRecoveryKey(cmd.Context(), path, func(recovery string) error {
		// The only secret output bypasses command/log writers and goes directly
		// to the verified terminal, never to redirected stdout or a service log.
		_, err := fmt.Fprintf(os.Stdout, "Save this key somewhere safe. Without it, your backups cannot be restored. We do not have a copy.\nRecovery key (shown once):\n%s\n", recovery)
		return err
	}, func(existing bool, k key.Key) error {
		if existing {
			fmt.Fprintln(cmd.OutOrStdout(), "Existing recovery key preserved; it will not be displayed. Use your saved copy, or copy the protected key file locally to safe offline storage before continuing.")
		}
		fmt.Fprint(cmd.OutOrStdout(), "Paste your saved recovery key (hidden): ")
		entered, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.OutOrStdout())
		if err != nil {
			return errors.New("cannot read recovery-key confirmation")
		}

		matches := k.Matches(string(entered))
		clear(entered)
		if !matches {
			return errors.New("saved recovery key does not match")
		}

		answer, err := prompt(input, cmd.OutOrStdout(), "Type SAVED to confirm this key is stored safely outside this machine", "")
		if err != nil || strings.TrimSpace(answer) != "SAVED" {
			return errors.New("recovery key was not acknowledged")
		}

		return nil
	})
	if err == nil {
		fmt.Fprintln(cmd.OutOrStdout(), "Recovery key acknowledged and saved privately. No backup has been executed.")
	}

	return err
}

func setupRecoveryKey(ctx context.Context, path string, show func(string) error, confirm func(bool, key.Key) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	settings, err := config.LoadDiagnostic(path)
	if err != nil {
		return err
	}

	keyPath, err := config.ResolveKeyPath(path, settings.Encryption.KeyFile)
	if err != nil {
		return err
	}

	k, created, err := key.Ensure(keyPath, settings.Encryption.KeyFile == "" && settings.Encryption.KeyID == "")
	if err != nil {
		return err
	}
	if settings.Encryption.KeyID != "" && settings.Encryption.KeyID != k.ID() {
		return errors.New("recovery key does not match the configured fingerprint; restore the original key; no key was changed")
	}
	if settings.Encryption.KeyAcknowledged && settings.Encryption.KeyID == k.ID() {
		return config.SaveKeyBinding(path, keyPath, k.ID(), true)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Binding precedes display. On any interruption the existing file consumes
	// the display opportunity, even if a terminal write was partial or failed.
	if err := config.SaveKeyBinding(path, keyPath, k.ID(), false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if created {
		if err := show(k.RecoveryString()); err != nil {
			return errors.New("cannot display recovery key; the saved key is preserved and will not be displayed again; resume with db-agent configure --key")
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := confirm(!created, k); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		return errors.New("recovery key was not confirmed and acknowledged; the saved key is preserved and will not be displayed again; resume with db-agent configure --key")
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	current, err := key.Load(keyPath)
	if err != nil {
		return err
	}
	if current.ID() != k.ID() {
		return errors.New("recovery key changed during setup; acknowledgement was not saved")
	}

	return config.SaveKeyBinding(path, keyPath, k.ID(), true)
}
