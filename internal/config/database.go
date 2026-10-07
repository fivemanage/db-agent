package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SaveDatabase preserves unrelated settings, including fields from newer agents.
func SaveDatabase(path string, database DatabaseConfig) error {
	if err := database.Validate(); err != nil {
		return err
	}
	if len(database.Databases) == 0 {
		return errors.New("confirm at least one database before saving")
	}

	return updateDocument(path, func(root *yaml.Node) error {
		return setYAML(root, "database", database)
	})
}

func updateDocument(path string, update func(*yaml.Node) error) error {
	return updateDocumentWith(path, update, replacePrivateFile)
}

func updateDocumentWith(path string, update func(*yaml.Node) error, replace func(string, string) error) error {
	path = ResolvePath(path)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("config must be an existing regular file; check its path and permissions")
	}

	original, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read config; check file permissions")
	}

	var document yaml.Node
	if err := yaml.Unmarshal(original, &document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("config must contain a valid YAML mapping")
	}

	root := document.Content[0]
	if err := update(root); err != nil {
		return err
	}

	contents, err := yaml.Marshal(&document)
	if err != nil {
		return errors.New("cannot encode config")
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate config staging name: %w", err)
	}

	staged := filepath.Join(filepath.Dir(path), ".db-agent-"+hex.EncodeToString(nonce[:])+".yaml")
	file, err := createPrivateFile(staged)
	if err != nil {
		return fmt.Errorf("create private config: %w", err)
	}
	defer os.Remove(staged)

	if _, err := file.Write(contents); err != nil {
		file.Close()
		return fmt.Errorf("write private config: %w", err)
	}

	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync private config: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close private config: %w", err)
	}

	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		return errors.New("config changed during setup; rerun configure to avoid overwriting changes")
	}
	if err := replace(staged, path); err != nil {
		return fmt.Errorf("replace private config: %w", err)
	}

	return nil
}
