package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type githubRoundTrip func(*http.Request) (*http.Response, error)

func (f githubRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Keep production URLs and redirect policy intact while routing all traffic to
// an isolated TLS server, including GitHub's separate release-asset host.
func githubTestHTTP(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("GitHub request received credentials")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	transport := client.Transport
	local, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = githubRoundTrip(func(req *http.Request) (*http.Response, error) {
		routed := req.Clone(req.Context())
		routed.URL.Scheme, routed.URL.Host = local.Scheme, local.Host
		routed.Host = req.URL.Host
		res, err := transport.RoundTrip(routed)
		if res != nil {
			res.Request = req
		}
		return res, err
	})
	return client
}

func githubFixture(t *testing.T, version, goos, goarch string) (Client, Manifest, ed25519.PrivateKey, githubRelease) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256([]byte("verified release binary"))
	artifact := "db-agent-" + version + "-" + goos + "-" + goarch
	if goos == "windows" {
		artifact += ".exe"
	}
	manifest := sign(t, Manifest{Version: version, OS: goos, Arch: goarch, URL: githubDownloads + "v" + version + "/" + artifact, SHA256: hex.EncodeToString(checksum[:])}, private)
	published := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	release := githubRelease{TagName: "v" + version, PublishedAt: &published, Prerelease: strings.Contains(version, "-rc.")}
	client := Client{PublicKey: public, Version: "1.0.0", OS: goos, Arch: goarch, Credential: "must-not-reach-github"}
	return client, manifest, private, release
}

func TestGitHubLatestStableCheckAndStage(t *testing.T) {
	for _, platform := range [][2]string{{"windows", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		t.Run(platform[0]+"-"+platform[1], func(t *testing.T) {
			client, manifest, _, release := githubFixture(t, "1.1.0", platform[0], platform[1])
			requests := 0
			client.HTTP = githubTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch r.Host + r.URL.Path {
				case "api.github.com/repos/" + githubRepository + "/releases/latest":
					if r.URL.RawQuery != "" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" {
						t.Error("unexpected GitHub API request")
					}
					json.NewEncoder(w).Encode(release)
				case "github.com/" + githubRepository + "/releases/download/v1.1.0/" + platform[0] + "-" + platform[1] + ".json":
					if r.URL.RawQuery != "" {
						t.Error("local settings were added to the asset request")
					}
					http.Redirect(w, r, "https://release-assets.githubusercontent.com/manifest", http.StatusFound)
				case "release-assets.githubusercontent.com/manifest":
					json.NewEncoder(w).Encode(manifest)
				default:
					if "https://"+r.Host+r.URL.Path != manifest.URL {
						t.Errorf("unexpected request: %s%s", r.Host, r.URL.Path)
						http.NotFound(w, r)
						return
					}
					w.Write([]byte("verified release binary"))
				}
			})

			got, newer, err := client.Check(context.Background())
			if err != nil || !newer || got != manifest {
				t.Fatalf("Check() = %+v, %v, %v", got, newer, err)
			}
			staged, err := client.Stage(context.Background(), got, filepath.Join(t.TempDir(), "agent"))
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(staged)
			if err != nil || string(contents) != "verified release binary" || requests != 4 {
				t.Fatalf("staged contents=%q, requests=%d, error=%v", contents, requests, err)
			}
		})
	}
}

func TestGitHubStagingSelectsNewestNumericCandidateAcrossPages(t *testing.T) {
	client, manifest, _, candidate := githubFixture(t, "1.1.0-rc.10", "windows", "amd64")
	client.Version = "1.1.0-rc.1"
	stable := candidate
	stable.TagName, stable.Prerelease = "v99.0.0", false
	first := make([]githubRelease, releasesPerPage)
	for i := range first {
		first[i] = stable
	}
	first[0] = candidate
	first[0].TagName = "v1.1.0-rc.2"
	draft, unpublished, malformed, mislabeled := candidate, candidate, candidate, candidate
	draft.TagName, draft.Draft = "v99.0.0-rc.1", true
	unpublished.TagName, unpublished.PublishedAt = "v98.0.0-rc.1", nil
	malformed.TagName = "v97.0.0-beta.1"
	mislabeled.TagName = "v96.0.0"
	pages, manifests := 0, 0
	client.HTTP = githubTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + githubRepository + "/releases":
			pages++
			if r.URL.Query().Get("per_page") != "100" {
				t.Error("unexpected pagination size")
			}
			switch r.URL.Query().Get("page") {
			case "1":
				json.NewEncoder(w).Encode(first)
			case "2":
				json.NewEncoder(w).Encode([]githubRelease{draft, unpublished, malformed, mislabeled, candidate, first[0]})
			default:
				t.Error("unexpected page")
			}
		case "/" + githubRepository + "/releases/download/v1.1.0-rc.10/windows-amd64.json":
			manifests++
			json.NewEncoder(w).Encode(manifest)
		default:
			t.Errorf("unexpected staging request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	got, newer, err := client.Check(context.Background())
	if err != nil || !newer || got != manifest || pages != 2 || manifests != 1 {
		t.Fatalf("Check() = %+v, %v, %v; pages=%d manifests=%d", got, newer, err, pages, manifests)
	}
}

func TestGitHubNoUpdateNeverDownloadsAnArtifact(t *testing.T) {
	for _, scenario := range []string{"no stable", "no staging", "same stable", "older stable", "same staging", "older staging"} {
		t.Run(scenario, func(t *testing.T) {
			client, _, _, release := githubFixture(t, "1.1.0", "linux", "amd64")
			client.Version = "1.1.0"
			if strings.Contains(scenario, "staging") {
				client.Version, release.TagName, release.Prerelease = "1.1.0-rc.10", "v1.1.0-rc.10", true
			}
			if strings.HasPrefix(scenario, "older") {
				release.TagName = "v1.0.0"
				if release.Prerelease {
					release.TagName += "-rc.1"
				}
			}
			client.HTTP = githubTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "api.github.com" {
					t.Error("download attempted without a newer release")
					http.Error(w, "unexpected download", http.StatusBadRequest)
					return
				}
				switch scenario {
				case "no stable":
					http.NotFound(w, r)
				case "no staging":
					json.NewEncoder(w).Encode([]githubRelease{})
				default:
					if release.Prerelease {
						json.NewEncoder(w).Encode([]githubRelease{release})
					} else {
						json.NewEncoder(w).Encode(release)
					}
				}
			})

			_, newer, err := client.Check(context.Background())
			if err != nil || newer {
				t.Fatalf("Check() = %v, %v", newer, err)
			}
		})
	}
}

func TestGitHubRejectsInvalidLatestRelease(t *testing.T) {
	for _, scenario := range []string{"draft", "unpublished", "prerelease", "wrong channel", "invalid version"} {
		t.Run(scenario, func(t *testing.T) {
			client, _, _, release := githubFixture(t, "1.1.0", "linux", "amd64")
			switch scenario {
			case "draft":
				release.Draft = true
			case "unpublished":
				release.PublishedAt = nil
			case "prerelease":
				release.Prerelease = true
			case "wrong channel":
				release.TagName = "v1.1.0-rc.1"
			case "invalid version":
				release.TagName = "../../other-repository"
			}
			client.HTTP = githubTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "api.github.com" {
					t.Error("invalid release triggered a download")
				}
				json.NewEncoder(w).Encode(release)
			})

			if _, _, err := client.Check(context.Background()); err == nil {
				t.Fatal("invalid latest release accepted")
			}
		})
	}
}

func TestGitHubRequiresMatchingSignedReleaseManifest(t *testing.T) {
	for _, scenario := range []string{"signature", "wrong key", "platform", "channel", "release version", "artifact URL", "malformed", "trailing data", "missing", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			client, manifest, private, release := githubFixture(t, "1.1.0", "linux", "amd64")
			switch scenario {
			case "signature":
				manifest.SHA256 = strings.Repeat("0", 64)
			case "wrong key":
				client.PublicKey, _, _ = ed25519.GenerateKey(rand.Reader)
			case "platform":
				manifest.OS = "windows"
				manifest = sign(t, manifest, private)
			case "channel":
				manifest.Version = "1.1.0-rc.1"
				manifest = sign(t, manifest, private)
			case "release version":
				manifest.Version = "1.0.1"
				manifest = sign(t, manifest, private)
			case "artifact URL":
				manifest.URL = "https://example.com/other-release"
				manifest = sign(t, manifest, private)
			}
			client.HTTP = githubTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "api.github.com" {
					json.NewEncoder(w).Encode(release)
					return
				}
				switch scenario {
				case "malformed":
					fmt.Fprint(w, "not json")
				case "missing":
					http.NotFound(w, r)
				case "oversized":
					fmt.Fprint(w, strings.Repeat(" ", maxManifestBytes+1))
				default:
					json.NewEncoder(w).Encode(manifest)
					if scenario == "trailing data" {
						fmt.Fprint(w, "{}")
					}
				}
			})

			if _, _, err := client.Check(context.Background()); err == nil {
				t.Fatal("invalid release manifest accepted")
			}
		})
	}
}

func TestGitHubLookupFailuresDoNotFallBack(t *testing.T) {
	for _, scenario := range []string{"rate limited", "forbidden", "server error", "malformed", "trailing data", "oversized", "page limit"} {
		t.Run(scenario, func(t *testing.T) {
			client, _, _, release := githubFixture(t, "1.1.0", "linux", "amd64")
			if scenario == "page limit" {
				client.Version = "1.0.0-rc.1"
			}
			requests := 0
			client.HTTP = githubTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Host != "api.github.com" {
					t.Error("failed discovery attempted an asset download")
				}
				switch scenario {
				case "rate limited":
					w.WriteHeader(http.StatusTooManyRequests)
				case "forbidden":
					w.WriteHeader(http.StatusForbidden)
				case "server error":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "malformed":
					fmt.Fprint(w, "<html>not json</html>")
				case "trailing data":
					json.NewEncoder(w).Encode(release)
					fmt.Fprint(w, "{}")
				case "oversized":
					fmt.Fprint(w, strings.Repeat(" ", maxReleaseBytes+1))
				case "page limit":
					json.NewEncoder(w).Encode(make([]githubRelease, releasesPerPage))
				}
			})

			if _, _, err := client.Check(context.Background()); err == nil {
				t.Fatal("failed release lookup reported success")
			}
			if scenario == "page limit" && requests != maxReleasePages || scenario != "page limit" && requests != 1 {
				t.Fatalf("unexpected request count: %d", requests)
			}
		})
	}
}

func TestGitHubRedirectPolicy(t *testing.T) {
	for _, scenario := range []string{"API", "unknown host", "HTTP", "credentials", "port", "fragment", "loop"} {
		t.Run(scenario, func(t *testing.T) {
			client, _, _, release := githubFixture(t, "1.1.0", "linux", "amd64")
			client.HTTP = githubTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "api.github.com" && scenario != "API" {
					json.NewEncoder(w).Encode(release)
					return
				}
				target := "https://example.com/manifest"
				switch scenario {
				case "HTTP":
					target = "http://release-assets.githubusercontent.com/manifest"
				case "credentials":
					target = "https://user:password@release-assets.githubusercontent.com/manifest"
				case "port":
					target = "https://release-assets.githubusercontent.com:444/manifest"
				case "fragment":
					target = "https://release-assets.githubusercontent.com/manifest#fragment"
				case "loop":
					target = "https://release-assets.githubusercontent.com/manifest"
				}
				if r.Host != "api.github.com" && r.Host != "github.com" && scenario != "loop" {
					t.Error("unsafe redirect reached its target")
				}
				http.Redirect(w, r, target, http.StatusFound)
			})

			if _, _, err := client.Check(context.Background()); err == nil {
				t.Fatal("unsafe redirect accepted")
			}
		})
	}
}

func TestGitHubCheckHonorsCancellationAndRejectsUnconfiguredTrust(t *testing.T) {
	client, _, _, _ := githubFixture(t, "1.1.0", "linux", "amd64")
	client.HTTP = &http.Client{Transport: githubRoundTrip(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := client.Check(ctx); err == nil {
		t.Fatal("canceled check succeeded")
	}
	client.HTTP = &http.Client{Transport: githubRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("unconfigured updater made a request")
		return nil, context.Canceled
	})}
	client.Version = "dev"
	if _, _, err := client.Check(context.Background()); err == nil {
		t.Fatal("development version accepted")
	}
	client.Version, client.PublicKey = "1.0.0", nil
	if _, _, err := client.Check(context.Background()); err == nil {
		t.Fatal("missing signing key accepted")
	}
}
