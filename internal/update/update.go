package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fivemanage/db-agent/internal/releaseversion"
)

const (
	maxManifestBytes = 16 << 10
	maxArtifactBytes = 256 << 20
)

// Manifest is signed by the release process. The signing key never reaches an agent.
type Manifest struct {
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`
}

// Payload is the exact byte sequence signed by the release process.
func (m Manifest) Payload() []byte {
	return []byte("fivemanage-db-agent-update-v1\n" + m.Version + "\n" + m.OS + "\n" + m.Arch + "\n" + m.URL + "\n" + m.SHA256 + "\n")
}

func (m Manifest) Verify(publicKey ed25519.PublicKey, currentVersion, goos, goarch string) (bool, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return false, errors.New("update signing public key is missing or invalid")
	}

	if m.OS != goos || m.Arch != goarch {
		return false, fmt.Errorf("update targets %s/%s, not %s/%s", m.OS, m.Arch, goos, goarch)
	}

	if err := requireHTTPS(m.URL); err != nil {
		return false, fmt.Errorf("artifact URL: %w", err)
	}

	checksum, err := hex.DecodeString(m.SHA256)
	if err != nil || len(checksum) != sha256.Size || strings.ToLower(m.SHA256) != m.SHA256 {
		return false, errors.New("update checksum must be a lowercase SHA-256 hex digest")
	}

	signature, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, m.Payload(), signature) {
		return false, errors.New("update manifest signature is invalid")
	}

	comparison, err := compareVersions(m.Version, currentVersion)
	if err != nil {
		return false, err
	}

	return comparison > 0, nil
}

func PublicKey(encoded string) (ed25519.PublicKey, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("update signing public key must be base64-encoded Ed25519")
	}

	return ed25519.PublicKey(key), nil
}

type Client struct {
	HTTP        *http.Client
	PublicKey   ed25519.PublicKey
	Version     string
	OS          string
	Arch        string
	ManifestURL string
	Credential  string
}

func (c Client) platform() (string, string) {
	goos, goarch := c.OS, c.Arch
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	return goos, goarch
}

func (c Client) httpClient() *http.Client {
	client := &http.Client{Timeout: 10 * time.Minute}
	if c.HTTP != nil {
		copy := *c.HTTP
		client = &copy
	}

	priorCheck := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := requireHTTPS(req.URL.String()); err != nil {
			return err
		}

		if len(via) >= 5 {
			return errors.New("too many update redirects")
		}

		if priorCheck != nil {
			return priorCheck(req, via)
		}

		return nil
	}

	return client
}

func (c Client) Check(ctx context.Context) (Manifest, bool, error) {
	if c.ManifestURL == "" {
		return c.checkGitHub(ctx)
	}

	if err := requireHTTPS(c.ManifestURL); err != nil {
		return Manifest{}, false, fmt.Errorf("manifest URL: %w", err)
	}

	u, _ := url.Parse(c.ManifestURL)
	goos, goarch := c.platform()
	query := u.Query()
	query.Set("os", goos)
	query.Set("arch", goarch)
	query.Set("current_version", c.Version)
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Manifest{}, false, err
	}
	if c.Credential != "" {
		req.Header.Set("Authorization", "Bearer "+c.Credential)
	}

	res, err := c.httpClient().Do(req)
	if err != nil {
		return Manifest{}, false, fmt.Errorf("fetch update manifest: %w", err)
	}
	defer res.Body.Close()

	if err := requireHTTPS(res.Request.URL.String()); err != nil {
		return Manifest{}, false, fmt.Errorf("manifest response: %w", err)
	}

	if res.Request.URL.Host != u.Host {
		return Manifest{}, false, errors.New("update manifest redirected to another host")
	}

	if res.StatusCode != http.StatusOK {
		return Manifest{}, false, fmt.Errorf("update manifest returned HTTP %d", res.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxManifestBytes+1))
	if err != nil {
		return Manifest{}, false, err
	}
	if len(body) > maxManifestBytes {
		return Manifest{}, false, errors.New("update manifest is too large")
	}

	manifest, err := decodeManifest(body)
	if err != nil {
		return Manifest{}, false, err
	}

	available, err := manifest.Verify(c.PublicKey, c.Version, goos, goarch)
	if err != nil {
		return Manifest{}, false, err
	}

	return manifest, available, nil
}

func decodeManifest(body []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode update manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("update manifest contains trailing data")
	}
	return manifest, nil
}

// Stage downloads a verified executable alongside the installed binary. The caller
// must keep the returned path private until the replacement helper runs.
func (c Client) Stage(ctx context.Context, manifest Manifest, executablePath string) (string, error) {
	goos, goarch := c.platform()
	available, err := manifest.Verify(c.PublicKey, c.Version, goos, goarch)
	if err != nil {
		return "", err
	}

	if !available {
		return "", errors.New("manifest does not contain a newer version")
	}

	if err := requireHTTPS(manifest.URL); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest.URL, nil)
	if err != nil {
		return "", err
	}

	res, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	defer res.Body.Close()

	if err := requireHTTPS(res.Request.URL.String()); err != nil {
		return "", fmt.Errorf("artifact response: %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("artifact returned HTTP %d", res.StatusCode)
	}

	if res.ContentLength > maxArtifactBytes {
		return "", errors.New("update artifact is too large")
	}

	suffix := ""
	if goos == "windows" {
		suffix = ".exe"
	}

	file, err := os.CreateTemp(filepath.Dir(executablePath), ".db-agent-stage-*"+suffix)
	if err != nil {
		return "", fmt.Errorf("stage update beside executable: %w", err)
	}
	path := file.Name()
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()

	if err = file.Chmod(0700); err != nil {
		return "", err
	}

	hash := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(res.Body, maxArtifactBytes+1))
	if copyErr != nil {
		err = fmt.Errorf("download update: %w", copyErr)
		return "", err
	}

	if count > maxArtifactBytes {
		err = errors.New("update artifact is too large")
		return "", err
	}

	if hex.EncodeToString(hash.Sum(nil)) != manifest.SHA256 {
		err = errors.New("update artifact checksum mismatch")
		return "", err
	}

	if err = file.Sync(); err != nil {
		return "", err
	}

	if err = file.Close(); err != nil {
		return "", err
	}

	complete = true

	return path, nil
}

func requireHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("HTTPS URL without embedded credentials or fragment required")
	}

	return nil
}

func compareVersions(a, b string) (int, error) {
	left, err := releaseversion.Parse(a)
	if err != nil {
		return 0, fmt.Errorf("update version: %w", err)
	}

	right, err := releaseversion.Parse(b)
	if err != nil {
		return 0, fmt.Errorf("installed version: %w", err)
	}

	// Signature trust alone must not turn a stable installation into a staging
	// installation (or vice versa), even if an operator reuses a signing key.
	if left.Channel() != right.Channel() {
		return 0, errors.New("update channel differs from the installed release; install the intended channel explicitly")
	}
	return left.Compare(right), nil
}
