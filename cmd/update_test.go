package cmd

import (
	"crypto/ed25519"
	"encoding/base64"
	"runtime"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
)

func TestUpdateClientDefaultsToGitHubAndPreservesExplicitFeed(t *testing.T) {
	previousKey := updatePublicKey
	updatePublicKey = base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	t.Cleanup(func() { updatePublicKey = previousKey })
	for _, test := range []struct{ base, override, want string }{
		{"", "", ""},
		{"http://localhost:3000", "", ""},
		{"https://example.com/base/", "", ""},
		{"http://localhost:3000", "https://releases.example.com/{os}-{arch}.json", "https://releases.example.com/" + runtime.GOOS + "-" + runtime.GOARCH + ".json"},
	} {
		client, err := newUpdateClient(config.Config{APIURL: test.base, UpdateURL: test.override, AgentCredential: "must-not-reach-updates"})
		if err != nil || client.ManifestURL != test.want || client.Credential != "" {
			t.Fatalf("unexpected manifest URL: %q err=%v", client.ManifestURL, err)
		}
	}
}
