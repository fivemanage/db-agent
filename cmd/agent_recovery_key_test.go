package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/setup"
)

func TestRecoveryKeySyncRequiresOptInAndUploadsTheBoundKey(t *testing.T) {
	path := setupConfig(t)
	setupStatus, err := setup.Status(path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := key.Load(setupStatus.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	credential := "dac_" + strings.Repeat("A", 43)
	status := make(chan controlplane.RecoveryKeyStatus, 1)
	uploaded := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "/api/v1/agent/recovery-key" || r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("managed copy used the wrong endpoint or missing agent authorization")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			select {
			case current := <-status:
				fmt.Fprintf(w, `{"status":%q}`, current)
			default:
				t.Error("unexpected status request")
				w.WriteHeader(http.StatusBadRequest)
			}
		case http.MethodPost:
			var body struct {
				Key string `json:"key"`
			}
			uploaded <- json.NewDecoder(r.Body).Decode(&body) == nil && original.Matches(body.Key)
			fmt.Fprint(w, `{"status":"saved"}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client, err := controlplane.New(server.URL, credential, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	status <- controlplane.RecoveryKeyDisabled
	if saved, err := syncRecoveryKey(context.Background(), path, client); saved || err != nil || len(uploaded) != 0 {
		t.Fatalf("disabled managed copy was not skipped: saved=%v err=%v", saved, err)
	}

	status <- controlplane.RecoveryKeyPending
	saved, err := syncRecoveryKey(context.Background(), path, client)
	if !saved || err != nil || len(uploaded) != 1 || !<-uploaded {
		t.Fatalf("the original bound key was not uploaded: saved=%v err=%v", saved, err)
	}
	settings, err := config.LoadDiagnostic(path)
	if err != nil || settings.Encryption.KeyID != original.ID() || settings.Encryption.KeyAcknowledged {
		t.Fatal("upload changed the local key binding or acknowledged setup")
	}
}

func TestRecoveryKeySyncDoesNotCreateOrSendAMismatchedKey(t *testing.T) {
	path := setupConfig(t)
	plane := &pendingRecoveryKeyPlane{}
	if saved, err := syncRecoveryKey(context.Background(), path, plane); saved || err != nil {
		t.Fatalf("an unconfigured key was not skipped: saved=%v err=%v", saved, err)
	}

	keyPath := filepath.Join(filepath.Dir(path), "wrong.key")
	otherKey, _, err := key.Ensure(keyPath, true)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("0", 64)
	if err := config.SaveKeyBinding(path, keyPath, fingerprint, true); err != nil {
		t.Fatal(err)
	}

	if saved, err := syncRecoveryKey(context.Background(), path, plane); saved || err == nil {
		t.Fatal("mismatched local key was accepted")
	}
	onDisk, err := key.Load(keyPath)
	if plane.uploads != 0 || err != nil || onDisk.ID() != otherKey.ID() {
		t.Fatal("sync uploaded or replaced the mismatched local key")
	}
}

type pendingRecoveryKeyPlane struct{ uploads int }

func (*pendingRecoveryKeyPlane) RecoveryKeyStatus(context.Context) (controlplane.RecoveryKeyStatus, error) {
	return controlplane.RecoveryKeyPending, nil
}

func (p *pendingRecoveryKeyPlane) UploadRecoveryKey(context.Context, key.Key) error {
	p.uploads++
	return nil
}
