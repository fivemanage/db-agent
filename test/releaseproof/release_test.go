//go:build release_smoke

package releaseproof

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/operation"
	"github.com/fivemanage/db-agent/internal/securefile"
	"github.com/fivemanage/db-agent/internal/update"
)

func TestReleaseArtifact(t *testing.T) {
	dir := os.Getenv("DB_AGENT_RELEASE_DIR")
	version := os.Getenv("DB_AGENT_RELEASE_VERSION")
	publicKey := os.Getenv("DB_AGENT_RELEASE_PUBLIC_KEY")
	if dir == "" || version == "" || publicKey == "" {
		t.Fatal("release smoke requires DB_AGENT_RELEASE_DIR, DB_AGENT_RELEASE_VERSION, and DB_AGENT_RELEASE_PUBLIC_KEY")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	artifact := filepath.Join(dir, "db-agent-"+version+"-"+runtime.GOOS+"-"+runtime.GOARCH+extension)
	run(t, artifact, "version", version)
	run(t, artifact, "--version", "db-agent version "+version)
	output, err := exec.Command(artifact, "version", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var buildMetadata struct {
		Version   string `json:"version"`
		PublicKey string `json:"public_key"`
		APIURL    string `json:"api_url"`
		GoVersion string `json:"go_version"`
		OS        string `json:"os"`
		Arch      string `json:"arch"`
	}
	if err := json.Unmarshal(output, &buildMetadata); err != nil {
		t.Fatal(err)
	}
	metadataBytes, err := os.ReadFile(filepath.Join(dir, "version.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		APIURL    string `json:"api_url"`
		GoVersion string `json:"go_version"`
	}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	if buildMetadata.Version != version || buildMetadata.PublicKey != publicKey || buildMetadata.APIURL != metadata.APIURL || buildMetadata.GoVersion != metadata.GoVersion || buildMetadata.OS != runtime.GOOS || buildMetadata.Arch != runtime.GOARCH {
		t.Fatalf("artifact build metadata does not match the release: %s", output)
	}

	data, err := os.ReadFile(filepath.Join(dir, runtime.GOOS+"-"+runtime.GOARCH+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest update.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	key, err := update.PublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if newer, err := manifest.Verify(key, "0.0.0", runtime.GOOS, runtime.GOARCH); err != nil || !newer || manifest.Version != version {
		t.Fatalf("release manifest: newer=%v, %v", newer, err)
	}

	work := t.TempDir()
	installed := filepath.Join(work, "db-agent"+extension)
	helper := filepath.Join(work, "helper"+extension)
	if baselinePath := os.Getenv("DB_AGENT_RELEASE_BASELINE"); baselinePath != "" {
		baseline, err := os.ReadFile(baselinePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(installed, baseline, 0700); err != nil {
			t.Fatal(err)
		}
	} else {
		modulePath, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			t.Fatal(err)
		}
		build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-X github.com/fivemanage/db-agent/cmd.version=0.0.0", "-o", installed, ".")
		build.Dir = filepath.Dir(strings.TrimSpace(string(modulePath)))
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build baseline: %v\n%s", err, output)
		}
	}
	run(t, installed, "version", "0.0.0")
	baseline, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, baseline, 0700); err != nil {
		t.Fatal(err)
	}

	// Keep signed public URLs intact. Only the test's transport substitutes local
	// bytes; production HTTP and trust policy are not weakened for CI.
	client := update.Client{PublicKey: key, Version: "0.0.0", HTTP: &http.Client{Transport: artifactTransport{path: artifact, url: manifest.URL}}}
	staged, err := client.Stage(context.Background(), manifest, installed)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(work, "config.yaml")
	args := []string{"update-helper", "--target", installed, "--staged", staged, "--sha256", manifest.SHA256, "--expected-version", version, "--restart-config", configPath}

	service := (runtime.GOOS == "linux" || runtime.GOOS == "windows") && os.Getenv("DB_AGENT_RELEASE_SERVICE_SMOKE") == "1"
	heartbeats := make(chan string, 16)
	credential := "dac_" + strings.Repeat("a", 43)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/register":
			fmt.Fprint(w, `{}`)
		case "/api/v1/agent/heartbeat":
			if r.Header.Get("Authorization") != "Bearer "+credential {
				t.Error("installed agent did not authenticate the heartbeat")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var body struct {
				Version string `json:"agent_version"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			select {
			case heartbeats <- body.Version:
			default:
			}
			fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	settings := fmt.Sprintf("api_url: %s\nagent_credential: %s\nauto_update: false\ndatabase:\n  host: 127.0.0.1\n  port: 1\n", server.URL, credential)
	if err := securefile.Create(configPath, []byte(settings)); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(work, "encryption.key")
	if err := securefile.Create(keyPath, []byte("preserved-release-smoke-key")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for path, expected := range map[string]string{configPath: settings, keyPath: "preserved-release-smoke-key"} {
			contents, err := securefile.Read(path, 1<<20)
			if err != nil || string(contents) != expected {
				t.Errorf("update/uninstall changed private state %s: %v", filepath.Base(path), err)
			}
		}
	})
	if service {
		assertNoService(t)
		t.Cleanup(func() {
			run(t, installed, "uninstall", "", "--config", configPath)
			assertNoService(t)
		})
		run(t, installed, "install", "", "--service-only", "--config", configPath)
		waitHeartbeat(t, heartbeats, "0.0.0")
		if runtime.GOOS == "linux" {
			assertLinuxUnit(t)
			if err := update.LaunchHelper(installed, staged, manifest.SHA256, version, update.Restart{ServiceName: "db-agent", ConfigPath: configPath}); err != nil {
				t.Fatal(err)
			}
			waitHeartbeat(t, heartbeats, version)
			waitHelperUnlock(t, configPath)
		} else {
			args = append(args, "--service", "db-agent")
			run(t, helper, args[0], "", args[1:]...)
			waitHeartbeat(t, heartbeats, version)
		}
	} else {
		t.Log("standalone replacement only; OS service smoke was not requested or is unsupported")
		args = append(args, "--no-restart")
		run(t, helper, args[0], "", args[1:]...)
		foregroundHeartbeat(t, installed, configPath, heartbeats, version)
	}
	run(t, installed, "version", version)
	rollback, err := os.ReadFile(installed + ".rollback")
	if err != nil || string(rollback) != string(baseline) {
		t.Fatalf("update did not retain the original rollback binary: %v", err)
	}
}

func assertLinuxUnit(t *testing.T) {
	t.Helper()
	if output, err := exec.Command("systemctl", "is-enabled", "db-agent.service").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "enabled" {
		t.Fatalf("service is not enabled for boot: %v, %s", err, output)
	}
	output, err := exec.Command("systemctl", "show", "db-agent.service", "--property=UMask,NoNewPrivileges,KillMode,User").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect service security settings: %v, %s", err, output)
	}
	for _, expected := range []string{"UMask=0077", "NoNewPrivileges=yes", "KillMode=control-group", "User=root"} {
		if !strings.Contains(string(output), expected+"\n") {
			t.Fatalf("systemd did not apply %q: %s", expected, output)
		}
	}
}

func waitHelperUnlock(t *testing.T, configPath string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		unlock, err := operation.TryLock(filepath.Join(filepath.Dir(configPath), "backup-transfer"))
		if err == nil {
			unlock()
			return
		}
		if !errors.Is(err, operation.ErrBusy) {
			t.Fatalf("inspect update helper lock: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("systemd update helper did not finish and release the operation lock")
}

func foregroundHeartbeat(t *testing.T, installed, configPath string, reports <-chan string, version string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, installed, "agent", "--config", configPath)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		command.Wait()
		if t.Failed() {
			t.Log(output.String())
		}
	}()
	waitHeartbeat(t, reports, version)
}

type artifactTransport struct{ path, url string }

func (a artifactTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != a.url || req.Header.Get("Authorization") != "" {
		return nil, fmt.Errorf("unexpected artifact request")
	}
	file, err := os.Open(a.path)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: file, Request: req}, nil
}

func run(t *testing.T, executable, command, expected string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, executable, append([]string{command}, args...)...)
	output, err := process.CombinedOutput()
	if err != nil || (expected != "" && strings.TrimSpace(string(output)) != expected) {
		t.Fatalf("%s: %v\n%s", command, err, output)
	}
}

func waitHeartbeat(t *testing.T, reports <-chan string, expected string) {
	t.Helper()
	timer := time.NewTimer(45 * time.Second)
	defer timer.Stop()
	for {
		select {
		case version := <-reports:
			if version == expected {
				return
			}
		case <-timer.C:
			t.Fatalf("installed agent did not heartbeat with version %s", expected)
		}
	}
}

func assertNoService(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		output, err := exec.Command("sc.exe", "query", "db-agent").CombinedOutput()
		if err == nil || !strings.Contains(string(output), "1060") {
			t.Fatalf("refusing to touch an existing or indeterminate service: %s, %v", output, err)
		}
		return
	}
	output, err := exec.Command("systemctl", "show", "db-agent.service", "--property=LoadState", "--value").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "not-found" {
		t.Fatalf("refusing to touch an existing or indeterminate systemd unit: %s, %v", output, err)
	}
}
