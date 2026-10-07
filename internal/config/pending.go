package config

import (
	"errors"
	"os"
	"strings"

	"github.com/fivemanage/db-agent/internal/securefile"
	"gopkg.in/yaml.v3"
)

type PendingEnrollment struct {
	APIURL     string `yaml:"api_url"`
	Credential string `yaml:"candidate_credential"`
}

func LoadPendingEnrollment(configPath string) (PendingEnrollment, error) {
	path := ResolvePath(configPath) + ".enrollment-pending"
	contents, err := securefile.Read(path, 4096)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PendingEnrollment{}, err
		}

		return PendingEnrollment{}, errors.New("cannot read pending enrollment; check local permissions")
	}

	var pending PendingEnrollment
	if yaml.Unmarshal(contents, &pending) != nil || pending.APIURL == "" || !strings.HasPrefix(pending.Credential, "dac_") || len(pending.Credential) != 47 {
		return PendingEnrollment{}, errors.New("pending enrollment is invalid; do not replace it or share its contents; contact support")
	}

	return pending, nil
}

func SavePendingEnrollment(configPath string, pending PendingEnrollment) error {
	path := ResolvePath(configPath) + ".enrollment-pending"
	contents, err := yaml.Marshal(pending)
	if err != nil {
		return errors.New("cannot encode pending enrollment")
	}

	if err := securefile.Create(path, contents); err != nil {
		return errors.New("cannot create pending enrollment; preserve existing pending material and retry")
	}

	return nil
}

func ClearPendingEnrollment(configPath string) error {
	err := os.Remove(ResolvePath(configPath) + ".enrollment-pending")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}
