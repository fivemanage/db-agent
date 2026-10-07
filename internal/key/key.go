package key

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fivemanage/db-agent/internal/securefile"
)

const Prefix = "dbak1_"

type Key struct{ material [32]byte }

func (Key) String() string   { return "[redacted recovery key]" }
func (Key) GoString() string { return "[redacted recovery key]" }
func (Key) Format(state fmt.State, _ rune) {
	fmt.Fprint(state, "[redacted recovery key]")
}

func (k Key) Bytes() [32]byte { return k.material }

// RecoveryString is only for the deliberate, one-time terminal display.
func (k Key) RecoveryString() string {
	return Prefix + base64.RawURLEncoding.EncodeToString(k.material[:])
}

func (k Key) ID() string {
	digest := sha256.Sum256(k.material[:])
	return hex.EncodeToString(digest[:])
}

func (k Key) Matches(saved string) bool {
	candidate, err := Parse(saved)
	return err == nil && subtle.ConstantTimeCompare(k.material[:], candidate.material[:]) == 1
}

func Parse(encoded string) (Key, error) {
	encoded = strings.TrimSpace(encoded)
	if len(encoded) != len(Prefix)+43 || !strings.HasPrefix(encoded, Prefix) {
		return Key{}, errors.New("recovery key has an unsupported format; preserve the existing file and restore the original recovery key")
	}

	decoded, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(encoded, Prefix))
	if err != nil || len(decoded) != 32 {
		return Key{}, errors.New("recovery key is invalid; preserve the existing file and restore the original recovery key")
	}

	var k Key
	copy(k.material[:], decoded)
	clear(decoded)

	return k, nil
}

func generate(random io.Reader) (Key, error) {
	var k Key
	if _, err := io.ReadFull(random, k.material[:]); err != nil {
		return Key{}, errors.New("cannot generate a secure recovery key")
	}

	return k, nil
}

func Load(path string) (Key, error) {
	contents, err := securefile.Read(path, 128)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Key{}, os.ErrNotExist
		}

		return Key{}, errors.New("cannot read a private recovery key; check the regular file, permissions, and service-account access; no key was changed")
	}
	defer clear(contents)

	return Parse(string(contents))
}

// Ensure may create only an unconfigured key. A configured but missing key must
// be restored, never replaced with new material that cannot decrypt old backups.
func Ensure(path string, allowCreate bool) (Key, bool, error) {
	k, err := Load(path)
	if err == nil {
		return k, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Key{}, false, err
	}
	if !allowCreate {
		return Key{}, false, errors.New("configured recovery key is missing; restore the original key from your safe copy; setup will not generate a replacement")
	}

	k, err = generate(rand.Reader)
	if err != nil {
		return Key{}, false, err
	}

	contents := []byte(k.RecoveryString() + "\n")
	defer clear(contents)
	if err := securefile.Create(path, contents); err != nil {
		// Another creator or an ambiguous publication failure may have left a
		// complete key. Recover it without granting another display opportunity.
		if existing, loadErr := Load(path); loadErr == nil {
			return existing, false, nil
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return Key{}, false, loadErr
		}

		return Key{}, false, errors.New("cannot save recovery key privately; preserve any existing key file and retry; no recovery key was displayed")
	}

	return k, true, nil
}
