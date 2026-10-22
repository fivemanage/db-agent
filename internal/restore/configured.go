package restore

import (
	"errors"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/key"
)

// ConfiguredKey deliberately does not validate DB reachability, dump tools, or
// backup policy: restoring must still work when the original database is broken.
func ConfiguredKey(configPath string, settings config.EncryptionConfig) (key.Key, error) {
	if settings.KeyFile == "" || settings.KeyID == "" || !settings.KeyAcknowledged {
		return key.Key{}, errors.New("local recovery key setup is incomplete; confirm the saved original key with db-agent configure --key; no key was generated or changed")
	}
	path, err := config.ResolveKeyPath(configPath, settings.KeyFile)
	if err != nil {
		return key.Key{}, err
	}
	k, err := key.Load(path)
	if err != nil || k.ID() != settings.KeyID {
		return key.Key{}, errors.New("cannot use the original private recovery key; check service-account access or restore its saved copy matching the configured fingerprint; no key was generated or changed")
	}
	return k, nil
}
