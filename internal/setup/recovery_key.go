package setup

import (
	"errors"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/key"
)

// BoundRecoveryKey reads the original private key without creating or
// acknowledging it. New setups can save a managed copy before finishing.
func BoundRecoveryKey(path string) (key.Key, bool, error) {
	settings, err := config.LoadDiagnostic(path)
	if err != nil {
		return key.Key{}, false, err
	}
	if settings.Encryption.KeyID == "" {
		return key.Key{}, false, nil
	}
	keyPath, err := config.ResolveKeyPath(path, settings.Encryption.KeyFile)
	if err != nil {
		return key.Key{}, false, err
	}
	k, err := key.Load(keyPath)
	if err != nil || k.ID() != settings.Encryption.KeyID {
		return key.Key{}, false, errors.New("cannot use the original private recovery key matching its saved fingerprint")
	}
	return k, true, nil
}
