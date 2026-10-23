//go:build windows_installer_smoke

package windowsinstaller

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/securefile"
	services "github.com/fivemanage/db-agent/internal/service"
	manager "github.com/kardianos/service"
)

func TestWindowsBootstrapServiceOnly(t *testing.T) {
	if os.Getenv("DB_AGENT_WINDOWS_INSTALLER_SMOKE") != "1" {
		t.Fatal("run only on a disposable Windows VM with DB_AGENT_WINDOWS_INSTALLER_SMOKE=1")
	}
	controller, err := services.NewController(&manager.Config{Name: services.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Status(); !errors.Is(err, manager.ErrNotInstalled) {
		t.Fatal("refusing to touch an existing or indeterminate db-agent service")
	}
	programFiles, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[Environment]::GetFolderPath('ProgramFiles')").Output()
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(strings.TrimSpace(string(programFiles)), "db-agent")
	if _, err := os.Lstat(installDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusing to touch an existing or indeterminate installation directory")
	}
	data, err := os.ReadFile(os.Getenv("DB_AGENT_WINDOWS_SMOKE_ARTIFACT"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	credential, err := controlplane.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	heartbeats := make(chan string, 4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/artifact":
			if r.Header.Get("Authorization") != "" {
				t.Error("artifact download received credentials")
			}
			w.Write(data)
		case "/api/v1/agent/register":
			fmt.Fprint(w, `{}`)
		case "/api/v1/agent/heartbeat":
			if r.Header.Get("Authorization") != "Bearer "+credential {
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
	work := t.TempDir()
	certPath := filepath.Join(work, "fixture.cer")
	certificate := server.Certificate().Raw
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0600); err != nil {
		t.Fatal(err)
	}
	// Windows addresses certificate-store entries by SHA-1 thumbprint; artifact
	// authenticity uses the independent SHA-256 pin, not this identifier.
	thumbprint := sha1.Sum(certificate)
	t.Setenv("DB_AGENT_FIXTURE_CERT", certPath)
	t.Setenv("DB_AGENT_FIXTURE_THUMBPRINT", strings.ToUpper(hex.EncodeToString(thumbprint[:])))
	runPS(t, `[void](Import-Certificate -FilePath $env:DB_AGENT_FIXTURE_CERT -CertStoreLocation Cert:\LocalMachine\Root -ErrorAction Stop)`)
	t.Cleanup(func() {
		runPS(t, `Remove-Item -LiteralPath ('Cert:\LocalMachine\Root\' + $env:DB_AGENT_FIXTURE_THUMBPRINT) -ErrorAction Stop`)
	})

	configPath := filepath.Join(work, "config.yaml")
	settings := []byte(fmt.Sprintf("api_url: %s\nagent_credential: %s\nauto_update: false\ndatabase:\n  host: 127.0.0.1\n  port: 1\n", server.URL, credential))
	if err := securefile.Create(configPath, settings); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(work, "encryption.key")
	key := []byte("preserved-windows-installer-proof-key")
	if err := securefile.Create(keyPath, key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := services.Control(controller, "uninstall"); err != nil {
			t.Error(err)
		}
		for path, expected := range map[string][]byte{configPath: settings, keyPath: key} {
			actual, err := securefile.Read(path, 1<<20)
			if err != nil || string(actual) != string(expected) {
				t.Errorf("installer/service removal changed private state: %s, %v", filepath.Base(path), err)
			}
		}
		binary := filepath.Join(installDir, "db-agent.exe")
		if actual, err := os.ReadFile(binary); err == nil && sha256.Sum256(actual) == digest {
			if err := os.Remove(binary); err != nil {
				t.Error(err)
			}
		}
		if err := os.Remove(installDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	})

	// The runtime fixture uses local HTTPS and the same three pinned fields.
	// Publisher-side signature verification is covered by the release tool tests.
	source, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install-windows.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for name, value := range map[string]string{"DbAgentReleaseVersion": "1.2.3", "DbAgentReleaseUrl": server.URL + "/artifact", "DbAgentReleaseSHA256": hex.EncodeToString(digest[:])} {
		placeholder := "$script:" + name + " = ''"
		if strings.Count(text, placeholder) != 1 {
			t.Fatal("unexpected installer fixture template")
		}
		text = strings.Replace(text, placeholder, "$script:"+name+" = '"+value+"'", 1)
	}
	installerPath := filepath.Join(work, "database.ps1")
	if err := os.WriteFile(installerPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_AGENT_FIXTURE_INSTALLER", installerPath)
	t.Setenv("DB_AGENT_FIXTURE_CONFIG", configPath)
	started := time.Now()
	runPS(t, `& ([scriptblock]::Create([IO.File]::ReadAllText($env:DB_AGENT_FIXTURE_INSTALLER))) -ServiceOnly -Config $env:DB_AGENT_FIXTURE_CONFIG`)
	select {
	case version := <-heartbeats:
		if version != "1.2.3" {
			t.Fatalf("installed service reports version %q", version)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("installed service did not authenticate its heartbeat")
	}
	t.Logf("native Windows bootstrap + service-only liveness: %s; not a full wizard/backup or two-minute customer qualification", time.Since(started))
}

func runPS(t *testing.T, script string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; "+script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native PowerShell fixture: %v\n%s", err, output)
	}
}
