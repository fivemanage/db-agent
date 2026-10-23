package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fivemanage/db-agent/internal/update"
)

func renderInstaller(args []string) error {
	flags := flag.NewFlagSet("render-installer", flag.ContinueOnError)
	platform := flags.String("platform", "linux", "installer platform: linux or windows")
	channel := flags.String("channel", "stable", "installer channel: stable or staging")
	source := flags.String("source", "", "unprovisioned installer template")
	output := flags.String("output", "", "new public installer output path")
	publicKey := flags.String("public-key", "", "independently trusted release public key")
	baseURL := flags.String("base-url", "", "public HTTPS GitHub release directory")
	manifestPath := flags.String("manifest", "", "signed windows/amd64 manifest (Windows only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" || flags.NArg() != 0 {
		return errors.New("render-installer requires --output, --public-key, and --base-url")
	}
	if *channel != "stable" && *channel != "staging" {
		return errors.New("installer channel must be stable or staging")
	}
	key, err := update.PublicKey(*publicKey)
	if err != nil {
		return err
	}
	values := make(map[string]string)
	quote := func(value string) string { return strings.ReplaceAll(value, "'", "'\"'\"'") }
	switch *platform {
	case "linux":
		if err := publicURL(*baseURL); err != nil {
			return fmt.Errorf("installer release base URL: %w", err)
		}
		values["release_public_key"] = base64.StdEncoding.EncodeToString(key)
		values["release_base_url"] = *baseURL
		values["release_channel"] = *channel
		if *source == "" {
			*source = "scripts/install-linux.sh"
		}
	case "windows":
		if *manifestPath == "" || *baseURL != "" {
			return errors.New("Windows rendering requires --manifest; the artifact URL must come from its signature, not --base-url")
		}
		var manifest update.Manifest
		if err := readJSON(*manifestPath, &manifest); err != nil {
			return fmt.Errorf("read signed Windows manifest: %w", err)
		}
		version, err := canonicalReleaseVersion(manifest.Version)
		if err != nil {
			return err
		}
		newer, err := manifest.Verify(key, version.Floor(), "windows", "amd64")
		if err != nil {
			return fmt.Errorf("verify Windows installer release: %w", err)
		}
		if !newer || version.Channel() != *channel {
			return errors.New("Windows installer release does not match the requested channel")
		}
		if err := publicURL(manifest.URL); err != nil {
			return fmt.Errorf("Windows artifact URL: %w", err)
		}
		values["$script:DbAgentReleaseVersion"] = manifest.Version
		values["$script:DbAgentReleaseUrl"] = manifest.URL
		values["$script:DbAgentReleaseSHA256"] = manifest.SHA256
		quote = func(value string) string { return strings.ReplaceAll(value, "'", "''") }
		if *source == "" {
			*source = "scripts/install-windows.ps1"
		}
	default:
		return errors.New("installer platform must be linux or windows")
	}
	contents, err := os.ReadFile(*source)
	if err != nil {
		return fmt.Errorf("read installer template: %w", err)
	}
	text := string(contents)
	for name, value := range values {
		placeholder := name + "=''"
		if name == "release_channel" {
			placeholder = "release_channel='stable'"
		}
		if *platform == "windows" {
			placeholder = name + " = ''"
		}
		if strings.Count(text, placeholder) != 1 {
			return errors.New("installer template must contain exactly one empty public-key and release-URL assignment")
		}
		assignment := name + "='" + quote(value) + "'"
		if *platform == "windows" {
			assignment = name + " = '" + quote(value) + "'"
		}
		text = strings.Replace(text, placeholder, assignment, 1)
	}
	return writeNew(*output, []byte(text))
}
