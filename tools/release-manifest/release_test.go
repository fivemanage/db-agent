package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestReleaseInputValidation(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base := releaseMetadata{Version: "1.2.3", PublicKey: base64.StdEncoding.EncodeToString(public), APIURL: "https://api.example.com"}
	if err := validateBuild(base); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", "dev", "v1.2.3", "01.2.3", "1.2.3-rc.1", "0.0.0", "18446744073709551616.0.0", "1.2.3 -X secret=value"} {
		m := base
		m.Version = version
		if err := validateBuild(m); err == nil {
			t.Errorf("accepted unsafe release version %q", version)
		}
	}
	for _, url := range []string{"http://api.example.com", "https://user:secret@api.example.com", "https://api.example.com?token=secret", "https://api.example.com#secret", "https://localhost", "https://127.0.0.1", "https://192.168.0.1", "https://api.internal", "https://api.local", "https://api.example.com/", "https://api.example.com -X secret=value"} {
		if err := publicURL(url); err == nil {
			t.Errorf("accepted unsafe public URL %q", url)
		}
	}
}

func TestBuildInspectionFailsClosed(t *testing.T) {
	base := debug.BuildInfo{Path: "github.com/fivemanage/db-agent", Settings: []debug.BuildSetting{
		{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"},
		{Key: "CGO_ENABLED", Value: "0"}, {Key: "-trimpath", Value: "true"},
	}}
	if err := inspectBuild(&base, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GOOS", "GOARCH", "CGO_ENABLED", "-trimpath"} {
		info := base
		info.Settings = append([]debug.BuildSetting(nil), base.Settings...)
		for i := range info.Settings {
			if info.Settings[i].Key == key {
				info.Settings[i].Value = "unexpected"
			}
		}
		if err := inspectBuild(&info, "linux", "amd64"); err == nil {
			t.Errorf("accepted unexpected %s", key)
		}
	}
	base.Deps = []*debug.Module{{Path: "example.com/private", Replace: &debug.Module{Path: "/private/source"}}}
	if err := inspectBuild(&base, "linux", "amd64"); err == nil {
		t.Fatal("accepted a dependency replacement")
	}
}

func TestChecksumSignatureAndInventory(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := releaseMetadata{Version: "1.2.3", Commit: strings.Repeat("a", 40), PublicKey: base64.StdEncoding.EncodeToString(public), APIURL: "https://api.example.com", BaseURL: "https://releases.example.com/v1.2.3"}
	if err := writeJSON(filepath.Join(dir, "version.json"), m); err != nil {
		t.Fatal(err)
	}
	for _, name := range releaseFiles(m.Version) {
		if name != "version.json" {
			if err := writeNew(filepath.Join(dir, name), []byte("fixture")); err != nil {
				t.Fatal(err)
			}
		}
	}
	sums, err := checksums(dir, releaseFiles(m.Version))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNew(filepath.Join(dir, "SHA256SUMS"), sums); err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(private, append([]byte(checksumDomain), sums...))
	if err := writeNew(filepath.Join(dir, "SHA256SUMS.sig"), []byte(base64.StdEncoding.EncodeToString(signature))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sbom.spdx.json"), []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(dir, public); err == nil || !strings.Contains(err.Error(), "checksums") {
		t.Fatalf("tampered artifact was not rejected by checksum: %v", err)
	}
	if err := writeNew(filepath.Join(dir, "secret.key"), []byte("must not publish")); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(dir, public); err == nil || !strings.Contains(err.Error(), "unexpected asset") {
		t.Fatalf("extra release file was not rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), append(sums, 'x'), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(dir, public); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("modified checksum list signature was accepted: %v", err)
	}
}

func TestMalformedSigningKeyRejected(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	private[len(private)-1] ^= 1
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(private)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKey(path); err == nil {
		t.Fatal("accepted inconsistent seed/public key")
	}
}

func TestSealReleaseRoundTrip(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	source := filepath.Join(work, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"go.mod":  "module github.com/fivemanage/db-agent\n\ngo 1.24\n",
		"main.go": "package main\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(work, "release")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, target := range releaseTargets {
		path := filepath.Join(dir, artifactName("1.2.3", target.OS, target.Arch))
		command := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", path, ".")
		command.Dir = source
		command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.OS, "GOARCH="+target.Arch, "GOWORK=off", "GOFLAGS=")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build fixture %s/%s: %v\n%s", target.OS, target.Arch, err, output)
		}
	}
	if err := writeNew(filepath.Join(dir, "sbom.spdx.json"), []byte(`{"spdxVersion":"SPDX-2.3","packages":[{"name":"test-fixture"}]}`)); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(work, "private.key")
	if err := os.WriteFile(privatePath, []byte(base64.StdEncoding.EncodeToString(private)), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"seal", "--dir", dir, "--private", privatePath, "--version", "1.2.3", "--public-key", base64.StdEncoding.EncodeToString(public), "--api-url", "https://api.example.com", "--commit", strings.Repeat("a", 40), "--base-url", "https://releases.example.com/1.2.3"}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify-release", "--dir", dir, "--public-key", base64.StdEncoding.EncodeToString(public)}); err != nil {
		t.Fatal(err)
	}
	if err := run(args); err == nil {
		t.Fatal("signing overwrote existing release metadata")
	}
	if err := os.Remove(filepath.Join(dir, artifactName("1.2.3", "windows", "amd64"))); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(dir, public); err == nil {
		t.Fatal("release verified with a missing required platform")
	}
}
