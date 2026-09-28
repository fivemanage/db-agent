package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func SaveEnrollment(path, apiURL, credential string) error {
	path = ResolvePath(path)
	if credential == "" {
		return errors.New("agent credential is required")
	}

	if err := validateAPIURL(apiURL); err != nil {
		return err
	}

	contents, err := yaml.Marshal(struct {
		AgentCredential string `yaml:"agent_credential"`
		APIURL          string `yaml:"api_url"`
	}{
		AgentCredential: credential,
		APIURL:          apiURL,
	})
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	file, err := createPrivateFile(path)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("config already exists at %s; enrollment will not overwrite it", path)
		}
		return fmt.Errorf("create private config: %w", err)
	}

	if _, err := file.Write(contents); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf("write config: %w", err)
	}

	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf("sync config: %w", err)
	}

	if err := file.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("close config: %w", err)
	}

	return nil
}
