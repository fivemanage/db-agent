package config

import (
	"encoding/hex"
	"errors"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func ResolveKeyPath(configPath, keyPath string) (string, error) {
	path, err := filepath.Abs(ResolvePath(configPath))
	if err != nil {
		return "", errors.New("cannot resolve private config directory")
	}

	if keyPath == "" {
		keyPath = "encryption.key"
	}
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(filepath.Dir(path), keyPath)
	}
	keyPath = filepath.Clean(keyPath)
	if keyPath == path || keyPath == path+".enrollment-pending" {
		return "", errors.New("recovery key must use a separate file, not config or pending enrollment")
	}

	return keyPath, nil
}

func validKeyID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(id) == 64 && len(decoded) == 32
}

// SaveKeyBinding stores only a path and fingerprint, never key material. A
// saved acknowledgement is not downgraded, and a different binding is rejected.
func SaveKeyBinding(path, keyPath, id string, acknowledged bool) error {
	if !filepath.IsAbs(keyPath) || !validKeyID(id) {
		return errors.New("recovery key binding requires an absolute path and valid fingerprint")
	}

	return updateDocument(path, func(root *yaml.Node) error {
		var encryption *yaml.Node
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "encryption" {
				encryption = root.Content[i+1]
			}
		}
		if encryption == nil {
			encryption = &yaml.Node{Kind: yaml.MappingNode}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "encryption"}, encryption)
		}

		var current EncryptionConfig
		if encryption.Kind != yaml.MappingNode || encryption.Decode(&current) != nil {
			return errors.New("encryption settings must be a valid YAML mapping")
		}

		if current.KeyFile != "" {
			existing, err := ResolveKeyPath(path, current.KeyFile)
			if err != nil || existing != keyPath {
				return errors.New("configured key path changed; setup will not replace the existing binding")
			}
		}
		if current.KeyID != "" && current.KeyID != id {
			return errors.New("configured key fingerprint does not match; restore the original key; setup will not rotate it")
		}
		if current.KeyID == id && current.KeyAcknowledged {
			acknowledged = true
		}

		for _, field := range []struct {
			name  string
			value any
		}{
			{"key_file", keyPath}, {"key_id", id}, {"key_acknowledged", acknowledged},
		} {
			if err := setYAML(encryption, field.name, field.value); err != nil {
				return err
			}
		}

		return nil
	})
}
