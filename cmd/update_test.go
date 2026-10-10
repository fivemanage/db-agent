package cmd

import (
	"crypto/ed25519"
	"encoding/base64"
	"runtime"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
)

func TestUpdateClientUsesAPIv1Feed(t *testing.T) {
	previousKey := updatePublicKey
	updatePublicKey = base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	t.Cleanup(func() { updatePublicKey = previousKey })
	for _, test := range []struct{ base, override, want string }{
		{"http://localhost:3000", "", "http://localhost:3000/api/v1/agent/updates"},
		{"http://localhost:3000/", "", "http://localhost:3000/api/v1/agent/updates"},
		{"https://example.com/base/", "", "https://example.com/base/api/v1/agent/updates"},
		{"http://localhost:3000", "https://releases.example.com/{os}-{arch}.json", "https://releases.example.com/" + runtime.GOOS + "-" + runtime.GOARCH + ".json"},
	} {
		client, err := newUpdateClient(config.Config{APIURL: test.base, UpdateURL: test.override})
		if err != nil || client.ManifestURL != test.want {
			t.Fatalf("unexpected manifest URL: %q err=%v", client.ManifestURL, err)
		}
	}
}
