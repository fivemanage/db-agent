package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/fivemanage/db-agent/internal/securefile"
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

	if err := securefile.Create(path, contents); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("config already exists at %s; enrollment will not overwrite it", path)
		}
		return fmt.Errorf("create private config: %w", err)
	}

	return nil
}
