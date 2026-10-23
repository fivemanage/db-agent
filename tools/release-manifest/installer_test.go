package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/update"
)

func TestRenderInstallerPinsPublicTrustWithoutPrivateMaterial(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.sh")
	output := filepath.Join(dir, "database.sh")
	if err := os.WriteFile(source, []byte("release_public_key=''\nrelease_base_url=''\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	args := []string{"render-installer", "--source", source, "--output", output, "--public-key", key, "--base-url", "https://releases.example.com/latest/download"}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "release_public_key='"+key+"'") || !strings.Contains(string(contents), "release_base_url='https://releases.example.com/latest/download'") {
		t.Fatal("public installer did not pin its complete trust and download settings")
	}
	if err := run(args); err == nil {
		t.Fatal("rendering overwrote an existing installer")
	}
	if err := os.WriteFile(source, contents, 0600); err != nil {
		t.Fatal(err)
	}
	args[4] = filepath.Join(dir, "second.sh")
	if err := run(args); err == nil {
		t.Fatal("rendering accepted an already provisioned or ambiguous source")
	}
}

func TestWindowsInstallerPinsOnlyVerifiedManifestValues(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := update.Manifest{Version: "1.2.3", OS: "windows", Arch: "amd64", URL: "https://github.com/fivemanage/db-agent/releases/download/v1.2.3/db-agent-1.2.3-windows-amd64.exe", SHA256: strings.Repeat("a", 64)}
	manifest.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(private, manifest.Payload()))
	for _, name := range []string{"valid", "tampered checksum", "wrong platform", "wrong key"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			value := manifest
			key := public
			switch name {
			case "tampered checksum":
				value.SHA256 = strings.Repeat("b", 64)
			case "wrong platform":
				value.OS = "linux"
				value.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(private, value.Payload()))
			case "wrong key":
				key, _, err = ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
			}
			manifestPath := filepath.Join(dir, "windows-amd64.json")
			if err := writeJSON(manifestPath, value); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "database.ps1")
			err := run([]string{"render-installer", "--platform", "windows", "--source", filepath.Join("..", "..", "scripts", "install-windows.ps1"), "--manifest", manifestPath, "--public-key", base64.StdEncoding.EncodeToString(key), "--output", output})
			if name != "valid" {
				if err == nil {
					t.Fatal("rendered an unverified Windows installer")
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("failed verification left a public installer output")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			for _, assignment := range []string{
				"$script:DbAgentReleaseVersion = '" + value.Version + "'",
				"$script:DbAgentReleaseUrl = '" + value.URL + "'",
				"$script:DbAgentReleaseSHA256 = '" + value.SHA256 + "'",
			} {
				if !strings.Contains(string(contents), assignment) {
					t.Errorf("published installer is missing %s", assignment)
				}
			}
		})
	}
}
