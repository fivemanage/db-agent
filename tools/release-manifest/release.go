package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/fivemanage/db-agent/internal/releaseversion"
	"github.com/fivemanage/db-agent/internal/update"
)

const checksumDomain = "fivemanage-db-agent-checksums-v1\n"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

var releaseTargets = []struct{ OS, Arch string }{
	{"windows", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"},
}

type releaseMetadata struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	PublicKey string `json:"public_key"`
	APIURL    string `json:"api_url"`
	BaseURL   string `json:"base_url"`
}

func runRelease(args []string) error {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := flags.String("dir", "", "release directory")
	private := flags.String("private", "", "private key file (seal only)")
	installerDir := flags.String("installer-dir", "scripts", "installer source directory (seal only)")
	m := releaseMetadata{}
	flags.StringVar(&m.Version, "version", "", "release version without v prefix; staging uses -rc.N")
	flags.StringVar(&m.PublicKey, "public-key", "", "trusted base64 public key")
	flags.StringVar(&m.APIURL, "api-url", "", "public channel-specific API base URL")
	flags.StringVar(&m.BaseURL, "base-url", "", "public immutable release directory URL")
	flags.StringVar(&m.Commit, "commit", "", "source commit SHA")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected release arguments")
	}
	if args[0] == "verify-release" {
		key, err := update.PublicKey(m.PublicKey)
		if err != nil {
			return err
		}
		return verifyRelease(*dir, key)
	}
	if args[0] == "channel" {
		version, err := canonicalReleaseVersion(m.Version)
		if err != nil {
			return err
		}
		fmt.Println(version.Channel())
		return nil
	}
	if err := validateBuild(m); err != nil {
		return err
	}
	if args[0] == "validate" {
		return nil
	}
	if *dir == "" || *private == "" || !commitPattern.MatchString(m.Commit) {
		return errors.New("seal requires --dir, --private, and a 40-character lowercase --commit")
	}
	if err := publicURL(m.BaseURL); err != nil {
		return fmt.Errorf("release base URL: %w", err)
	}
	key, err := readPrivateKey(*private)
	if err != nil {
		return err
	}
	if base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) != m.PublicKey {
		return errors.New("public key does not match release signing key")
	}
	return sealRelease(*dir, m, key, *installerDir)
}

func canonicalReleaseVersion(value string) (releaseversion.Version, error) {
	version, err := releaseversion.Parse(value)
	if err != nil {
		return version, err
	}
	if strings.HasPrefix(value, "v") || version.IsZero() {
		return version, errors.New("release version must be nonzero major.minor.patch or major.minor.patch-rc.N, without v prefix")
	}
	return version, nil
}

func validateBuild(m releaseMetadata) error {
	if _, err := canonicalReleaseVersion(m.Version); err != nil {
		return err
	}
	key, err := update.PublicKey(m.PublicKey)
	if err != nil {
		return err
	}
	if base64.StdEncoding.EncodeToString(key) != m.PublicKey {
		return errors.New("release public key must use canonical single-line base64")
	}
	if err := publicURL(m.APIURL); err != nil {
		return fmt.Errorf("release API URL: %w", err)
	}
	return nil
}

func publicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(raw, "/") || strings.ContainsAny(raw, " \t\r\n\"'\\") {
		return errors.New("public HTTPS URL without credentials, query, fragment, whitespace, or trailing slash required")
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return errors.New("public DNS hostname required, not a local hostname or IP address")
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".test", ".invalid"} {
		if strings.HasSuffix(host, suffix) {
			return errors.New("private or reserved hostname is not a public release URL")
		}
	}
	return nil
}

func artifactName(version, goos, arch string) string {
	name := "db-agent-" + version + "-" + goos + "-" + arch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func releaseFiles(version string) []string {
	names := []string{"version.json", "sbom.spdx.json"}
	for _, target := range releaseTargets {
		names = append(names, artifactName(version, target.OS, target.Arch), target.OS+"-"+target.Arch+".json")
	}
	sort.Strings(names)
	return names
}

func sealRelease(dir string, m releaseMetadata, key ed25519.PrivateKey, installerDir string) error {
	if err := inspectArtifacts(dir, &m); err != nil {
		return err
	}
	version, err := canonicalReleaseVersion(m.Version)
	if err != nil {
		return err
	}
	for _, target := range releaseTargets {
		name := artifactName(m.Version, target.OS, target.Arch)
		digest, err := fileDigest(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		manifest := update.Manifest{Version: m.Version, OS: target.OS, Arch: target.Arch, URL: m.BaseURL + "/" + name, SHA256: digest}
		manifest.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, manifest.Payload()))
		if _, err := manifest.Verify(key.Public().(ed25519.PublicKey), version.Floor(), target.OS, target.Arch); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, target.OS+"-"+target.Arch+".json"), manifest); err != nil {
			return err
		}
	}
	if err := renderInstaller([]string{"--platform", "linux", "--source", filepath.Join(installerDir, "install-linux.sh"), "--channel", version.Channel(), "--public-key", m.PublicKey, "--base-url", m.BaseURL, "--output", filepath.Join(dir, "database.sh")}); err != nil {
		return fmt.Errorf("render release Linux installer: %w", err)
	}
	if err := renderInstaller([]string{"--platform", "windows", "--source", filepath.Join(installerDir, "install-windows.ps1"), "--channel", version.Channel(), "--public-key", m.PublicKey, "--manifest", filepath.Join(dir, "windows-amd64.json"), "--output", filepath.Join(dir, "database.ps1")}); err != nil {
		return fmt.Errorf("render release Windows installer: %w", err)
	}
	if err := writeJSON(filepath.Join(dir, "version.json"), m); err != nil {
		return err
	}
	names, err := releaseInventory(dir, m.Version)
	if err != nil {
		return err
	}
	sums, err := checksums(dir, names)
	if err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, "SHA256SUMS"), sums); err != nil {
		return err
	}
	signature := ed25519.Sign(key, append([]byte(checksumDomain), sums...))
	if err := writeNew(filepath.Join(dir, "SHA256SUMS.sig"), []byte(base64.StdEncoding.EncodeToString(signature)+"\n")); err != nil {
		return err
	}
	if err := verifyRelease(dir, key.Public().(ed25519.PublicKey)); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		if bytes.Contains(data, key) || bytes.Contains(data, key.Seed()) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(key))) {
			return errors.New("release contains signing key material; refusing publication")
		}
	}
	return nil
}

func verifyRelease(dir string, key ed25519.PublicKey) error {
	if dir == "" {
		return errors.New("release directory is required")
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	encoded, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
	if err != nil {
		return err
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || !ed25519.Verify(key, append([]byte(checksumDomain), sums...), signature) {
		return errors.New("release checksum signature is invalid")
	}
	var m releaseMetadata
	if err := readJSON(filepath.Join(dir, "version.json"), &m); err != nil {
		return err
	}
	if err := validateBuild(m); err != nil {
		return err
	}
	allowed := append(releaseFiles(m.Version), "SHA256SUMS", "SHA256SUMS.sig", "provenance.sigstore.json", "database.sh", "database.ps1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		found := false
		for _, name := range allowed {
			found = found || entry.Name() == name
		}
		if !found || !entry.Type().IsRegular() {
			return errors.New("release contains an unexpected asset, directory, or link")
		}
	}
	if m.PublicKey != base64.StdEncoding.EncodeToString(key) || !commitPattern.MatchString(m.Commit) {
		return errors.New("release metadata does not match the trusted key or source commit format")
	}
	if err := publicURL(m.BaseURL); err != nil {
		return err
	}
	names, err := releaseInventory(dir, m.Version)
	if err != nil {
		return err
	}
	expected, err := checksums(dir, names)
	if err != nil {
		return err
	}
	if !bytes.Equal(sums, expected) {
		return errors.New("release checksums or file inventory do not match")
	}
	for _, target := range releaseTargets {
		var manifest update.Manifest
		if err := readJSON(filepath.Join(dir, target.OS+"-"+target.Arch+".json"), &manifest); err != nil {
			return err
		}
		version, err := canonicalReleaseVersion(m.Version)
		if err != nil {
			return err
		}
		if _, err := manifest.Verify(key, version.Floor(), target.OS, target.Arch); err != nil {
			return err
		}
		name := artifactName(m.Version, target.OS, target.Arch)
		digest, err := fileDigest(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if manifest.Version != m.Version || manifest.URL != m.BaseURL+"/"+name || manifest.SHA256 != digest {
			return errors.New("manifest does not match the release artifact")
		}
	}
	return inspectArtifacts(dir, &m)
}

func releaseInventory(dir, version string) ([]string, error) {
	names := releaseFiles(version)
	// Older bundles predate generated installers. New installers, when present,
	// must be covered by the signed inventory rather than merely allowed extras.
	for _, name := range []string{"database.sh", "database.ps1"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			names = append(names, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	sort.Strings(names)
	return names, nil
}

func inspectArtifacts(dir string, m *releaseMetadata) error {
	var sbom struct {
		Version  string            `json:"spdxVersion"`
		Packages []json.RawMessage `json:"packages"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "sbom.spdx.json"))
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &sbom) != nil || sbom.Version != "SPDX-2.3" || len(sbom.Packages) == 0 {
		return errors.New("release requires a nonempty SPDX 2.3 SBOM")
	}
	for _, target := range releaseTargets {
		name := artifactName(m.Version, target.OS, target.Arch)
		info, err := buildinfo.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if err := inspectBuild(info, target.OS, target.Arch); err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if m.GoVersion != "" && m.GoVersion != info.GoVersion {
			return errors.New("release artifacts use different Go toolchains")
		}
		m.GoVersion = info.GoVersion
	}
	return nil
}

func inspectBuild(info *debug.BuildInfo, goos, arch string) error {
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if info.Path != "github.com/fivemanage/db-agent" || settings["CGO_ENABLED"] != "0" || settings["GOOS"] != goos || settings["GOARCH"] != arch || settings["-trimpath"] != "true" {
		return errors.New("binary has unexpected target, CGO, or path build settings")
	}
	for _, dep := range info.Deps {
		if dep.Replace != nil {
			return errors.New("release cannot contain local or alternate dependency replacements")
		}
	}
	return nil
}

func checksums(dir string, names []string) ([]byte, error) {
	var output strings.Builder
	for _, name := range names {
		digest, err := fileDigest(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&output, "%s  %s\n", digest, name)
	}
	return []byte(output.String()), nil
}

func fileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("release assets must be regular files, not links or directories")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("release JSON contains trailing data")
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeNew(path, append(data, '\n'))
}

func writeNew(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
