package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sign(t *testing.T, manifest Manifest, privateKey ed25519.PrivateKey) Manifest {
	t.Helper()
	manifest.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest.Payload()))
	return manifest
}

func TestManifestVerification(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256([]byte("binary"))
	base := sign(t, Manifest{Version: "1.2.0", OS: "windows", Arch: "amd64", URL: "https://example.com/agent.exe", SHA256: hex.EncodeToString(checksum[:])}, privateKey)
	for _, test := range []struct {
		name      string
		manifest  Manifest
		current   string
		os        string
		wantNewer bool
		wantError bool
	}{
		{name: "newer", manifest: base, current: "1.1.9", os: "windows", wantNewer: true},
		{name: "same version", manifest: base, current: "1.2.0", os: "windows"},
		{name: "downgrade", manifest: base, current: "1.3.0", os: "windows"},
		{name: "wrong platform", manifest: base, current: "1.1.9", os: "linux", wantError: true},
		{name: "tampered version", manifest: func() Manifest { m := base; m.Version = "9.0.0"; return m }(), current: "1.1.9", os: "windows", wantError: true},
		{name: "insecure URL", manifest: func() Manifest { m := base; m.URL = "http://example.com/agent.exe"; return m }(), current: "1.1.9", os: "windows", wantError: true},
		{name: "invalid installed version", manifest: base, current: "dev", os: "windows", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			newer, err := test.manifest.Verify(publicKey, test.current, test.os, "amd64")
			if (err != nil) != test.wantError || (err == nil && newer != test.wantNewer) {
				t.Fatalf("Verify() = %v, %v; want newer=%v error=%v", newer, err, test.wantNewer, test.wantError)
			}
		})
	}
}

func TestCheckAndStageVerifiedArtifact(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("signed agent binary")
	checksum := sha256.Sum256(artifact)
	var manifest Manifest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest":
			if r.URL.Query().Get("os") != "linux" || r.URL.Query().Get("arch") != "amd64" || r.URL.Query().Get("current_version") != "1.0.0" {
				t.Errorf("unexpected manifest query: %s", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(manifest)
		case "/artifact":
			w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manifest = sign(t, Manifest{Version: "1.1.0", OS: "linux", Arch: "amd64", URL: server.URL + "/artifact", SHA256: hex.EncodeToString(checksum[:])}, privateKey)
	client := Client{HTTP: server.Client(), PublicKey: publicKey, Version: "1.0.0", OS: "linux", Arch: "amd64", ManifestURL: server.URL + "/manifest"}
	got, newer, err := client.Check(context.Background())
	if err != nil || !newer {
		t.Fatalf("Check() = %v, %v", newer, err)
	}
	path, err := client.Stage(context.Background(), got, filepath.Join(t.TempDir(), "db-agent"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != string(artifact) {
		t.Fatalf("staged artifact = %q, %v", contents, err)
	}
	got.SHA256 = strings.Repeat("0", 64)
	if _, err := client.Stage(context.Background(), got, filepath.Join(t.TempDir(), "db-agent")); err == nil {
		t.Fatal("Stage() accepted a modified manifest")
	}
}

func TestStageRejectsArtifactChecksumMismatchAndCleansUp(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("wrong binary"))
	}))
	defer server.Close()
	checksum := sha256.Sum256([]byte("expected binary"))
	manifest := sign(t, Manifest{Version: "1.1.0", OS: "linux", Arch: "amd64", URL: server.URL, SHA256: hex.EncodeToString(checksum[:])}, privateKey)
	client := Client{HTTP: server.Client(), PublicKey: publicKey, Version: "1.0.0", OS: "linux", Arch: "amd64"}
	dir := t.TempDir()
	if _, err := client.Stage(context.Background(), manifest, filepath.Join(dir, "db-agent")); err == nil {
		t.Fatal("Stage() accepted the wrong binary")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial stage files remain: %v, %v", entries, err)
	}
}
