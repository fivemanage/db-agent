package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	manager "github.com/kardianos/service"
)

type fakeController struct {
	status manager.Status
	err    error
	fail   error
	calls  []string
}

func (f *fakeController) Status() (manager.Status, error) { return f.status, f.err }
func (f *fakeController) Install() error {
	f.calls = append(f.calls, "install")
	if f.fail == nil {
		f.status, f.err = manager.StatusStopped, nil
	}
	return f.fail
}
func (f *fakeController) Start() error {
	f.calls = append(f.calls, "start")
	if f.fail == nil {
		f.status = manager.StatusRunning
	}
	return f.fail
}
func (f *fakeController) Stop() error {
	f.calls = append(f.calls, "stop")
	if f.fail == nil {
		f.status = manager.StatusStopped
	}
	return f.fail
}
func (f *fakeController) Uninstall() error {
	f.calls = append(f.calls, "uninstall")
	if f.fail == nil {
		f.status, f.err = manager.StatusUnknown, manager.ErrNotInstalled
	}
	return f.fail
}

func TestControlIsIdempotent(t *testing.T) {
	svc := &fakeController{err: manager.ErrNotInstalled}
	for _, action := range []string{"install", "install", "start", "start", "stop", "stop", "start", "uninstall", "uninstall", "stop"} {
		if message, err := Control(svc, action); err != nil || message == "" {
			t.Fatalf("%s: message=%q, err=%v", action, message, err)
		}
	}
	if !reflect.DeepEqual(svc.calls, []string{"install", "start", "stop", "start", "stop", "uninstall"}) {
		t.Fatalf("non-idempotent controls: %v", svc.calls)
	}
	if _, err := Control(svc, "start"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing service start: %v", err)
	}
}

func TestControlDoesNotMutateUnknownStateOrUninstallAfterFailedStop(t *testing.T) {
	for _, svc := range []*fakeController{
		{status: manager.StatusUnknown},
		{err: errors.New("manager unavailable")},
		{status: manager.StatusRunning, fail: os.ErrPermission},
	} {
		_, err := Control(svc, "uninstall")
		if err == nil {
			t.Fatal("unsafe uninstall accepted")
		}
		for _, call := range svc.calls {
			if call != "stop" {
				t.Fatalf("uninstalled after state/stop failure: %v", svc.calls)
			}
		}
		if errors.Is(err, os.ErrPermission) && !strings.Contains(err.Error(), "Administrator") {
			t.Fatalf("permission failure is not actionable: %v", err)
		}
	}
}

func TestUninstallPreservesAgentData(t *testing.T) {
	dir := t.TempDir()
	files := []string{"config.yaml", "encryption.key", "backup.enc", "agent.log", "config.yaml.enrollment-pending"}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("private "+file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc := &fakeController{status: manager.StatusRunning}
	if _, err := Control(svc, "uninstall"); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(data) != "private "+file {
			t.Fatalf("uninstall changed %s: %v", file, err)
		}
	}
}
