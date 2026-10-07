package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"gopkg.in/yaml.v3"
)

func DecodeDashboardOverrides(contents []byte) (DashboardOverrides, error) {
	contents = bytes.TrimSpace(contents)
	if len(contents) == 0 || contents[0] != '{' {
		return DashboardOverrides{}, errors.New("dashboard overrides must be an object containing only allowed fields")
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var overrides DashboardOverrides
	if decoder.Decode(&overrides) != nil {
		return DashboardOverrides{}, errors.New("dashboard overrides contain unknown fields or invalid types; local secrets and paths cannot be overridden")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return DashboardOverrides{}, errors.New("invalid dashboard overrides")
	}

	// Validate even before DB/key setup is complete; only backup policy is remote.
	baseline := newViper()
	var c Config
	if baseline.Unmarshal(&c) != nil {
		return DashboardOverrides{}, errors.New("cannot load configuration defaults")
	}

	c.AgentCredential = "validation-only"
	if _, err := c.WithDashboardOverrides(overrides); err != nil {
		return DashboardOverrides{}, errors.New("dashboard backup policy is invalid")
	}

	return overrides, nil
}

func SaveDashboardOverrides(path string, overrides DashboardOverrides) error {
	c, err := LoadDiagnostic(path)
	if err != nil {
		return err
	}

	if _, err := c.WithDashboardOverrides(overrides); err != nil {
		return err
	}

	return updateDocument(path, func(root *yaml.Node) error {
		if overrides.AutoUpdate != nil {
			if err := setYAML(root, "auto_update", *overrides.AutoUpdate); err != nil {
				return err
			}
		}

		b := overrides.Backup
		if b.Schedule == nil && b.RetentionDays == nil && b.Compression == nil && b.ThrottleIO == nil {
			return nil
		}

		var backup *yaml.Node
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "backup" {
				backup = root.Content[i+1]
			}
		}
		if backup == nil {
			backup = &yaml.Node{Kind: yaml.MappingNode}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "backup"}, backup)
		}
		if backup.Kind != yaml.MappingNode {
			return errors.New("backup settings must be a YAML mapping")
		}

		if b.Schedule != nil {
			if err := setYAML(backup, "schedule", *b.Schedule); err != nil {
				return err
			}
		}

		if b.RetentionDays != nil {
			if err := setYAML(backup, "retention_days", *b.RetentionDays); err != nil {
				return err
			}
		}

		if b.Compression != nil {
			if err := setYAML(backup, "compression", *b.Compression); err != nil {
				return err
			}
		}

		if b.ThrottleIO != nil {
			if err := setYAML(backup, "throttle_io", *b.ThrottleIO); err != nil {
				return err
			}
		}

		return nil
	})
}

func setYAML(root *yaml.Node, key string, value any) error {
	var encoded yaml.Node
	if err := encoded.Encode(value); err != nil {
		return errors.New("cannot encode private configuration field")
	}

	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = &encoded
			return nil
		}
	}

	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &encoded)

	return nil
}
