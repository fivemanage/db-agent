package update

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fivemanage/db-agent/internal/releaseversion"
)

const (
	githubRepository = "fivemanage/db-agent"
	githubReleases   = "https://api.github.com/repos/" + githubRepository + "/releases"
	githubDownloads  = "https://github.com/" + githubRepository + "/releases/download/"
	maxReleaseBytes  = 4 << 20
	maxReleasePages  = 10
	releasesPerPage  = 100
)

type githubRelease struct {
	TagName     string     `json:"tag_name"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt *time.Time `json:"published_at"`
}

func (c Client) checkGitHub(parent context.Context) (Manifest, bool, error) {
	if len(c.PublicKey) != ed25519.PublicKeySize {
		return Manifest{}, false, errors.New("update signing public key is missing or invalid")
	}
	current, err := releaseversion.Parse(c.Version)
	if err != nil {
		return Manifest{}, false, fmt.Errorf("installed version: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()

	release, found, err := c.githubRelease(ctx, current.Channel())
	if err != nil || !found {
		return Manifest{}, false, err
	}
	next, err := releaseversion.Parse(release.TagName)
	if err != nil || next.Channel() != current.Channel() || release.Draft || release.PublishedAt == nil || release.Prerelease != (current.Channel() == "staging") {
		return Manifest{}, false, errors.New("GitHub release does not match the installed update channel")
	}
	if next.Compare(current) <= 0 {
		return Manifest{}, false, nil
	}

	goos, goarch := c.platform()
	base := githubDownloads + release.TagName + "/"
	body, _, err := c.fetchGitHub(ctx, base+goos+"-"+goarch+".json", maxManifestBytes, false)
	if err != nil {
		return Manifest{}, false, err
	}
	manifest, err := decodeManifest(body)
	if err != nil {
		return Manifest{}, false, err
	}
	available, err := manifest.Verify(c.PublicKey, c.Version, goos, goarch)
	if err != nil {
		return Manifest{}, false, err
	}

	// GitHub selects a release, but cannot substitute another release's signed
	// manifest or an artifact outside that exact release.
	version := strings.TrimPrefix(release.TagName, "v")
	artifact := "db-agent-" + version + "-" + goos + "-" + goarch
	if goos == "windows" {
		artifact += ".exe"
	}
	if manifest.Version != version || manifest.URL != base+artifact {
		return Manifest{}, false, errors.New("signed update manifest does not match the selected GitHub release")
	}
	return manifest, available, nil
}

func (c Client) githubRelease(ctx context.Context, channel string) (githubRelease, bool, error) {
	if channel == "stable" {
		body, status, err := c.fetchGitHub(ctx, githubReleases+"/latest", maxReleaseBytes, true)
		if err != nil || status == http.StatusNotFound {
			return githubRelease{}, false, err
		}
		var release githubRelease
		if err := json.Unmarshal(body, &release); err != nil {
			return githubRelease{}, false, errors.New("GitHub returned invalid release metadata")
		}
		return release, true, nil
	}

	var selected githubRelease
	var newest releaseversion.Version
	found := false
	for page := 1; page <= maxReleasePages; page++ {
		body, _, err := c.fetchGitHub(ctx, fmt.Sprintf("%s?per_page=%d&page=%d", githubReleases, releasesPerPage, page), maxReleaseBytes, true)
		if err != nil {
			return githubRelease{}, false, err
		}
		var releases []githubRelease
		if err := json.Unmarshal(body, &releases); err != nil {
			return githubRelease{}, false, errors.New("GitHub returned invalid release metadata")
		}
		for _, release := range releases {
			version, err := releaseversion.Parse(release.TagName)
			if err != nil || version.Channel() != "staging" || !release.Prerelease || release.Draft || release.PublishedAt == nil {
				continue
			}
			if !found || version.Compare(newest) > 0 {
				selected, newest, found = release, version, true
			}
		}
		if len(releases) < releasesPerPage {
			return selected, found, nil
		}
	}
	return githubRelease{}, false, errors.New("GitHub release history exceeds the bounded staging lookup; configure update_url explicitly")
}

func (c Client) fetchGitHub(ctx context.Context, rawURL string, limit int64, api bool) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "fivemanage-db-agent")
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}

	// Public discovery never uses an agent credential, GitHub token, or cookies.
	// Only GitHub asset downloads may redirect to its release-storage hosts.
	client := c.httpClient()
	client.Jar = nil
	priorCheck := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if api || !githubAssetHost(req.URL) {
			return errors.New("unexpected GitHub update redirect")
		}
		return priorCheck(req, via)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch GitHub update metadata: %w", err)
	}
	defer res.Body.Close()
	if api && res.StatusCode == http.StatusNotFound && rawURL == githubReleases+"/latest" {
		return nil, res.StatusCode, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, res.StatusCode, fmt.Errorf("GitHub update metadata returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, res.StatusCode, fmt.Errorf("read GitHub update metadata: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, res.StatusCode, errors.New("GitHub update metadata is too large")
	}
	return body, res.StatusCode, nil
}

func githubAssetHost(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" {
		return false
	}
	switch u.Host {
	case "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	default:
		return false
	}
}
