package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	services "github.com/fivemanage/db-agent/internal/service"
	manager "github.com/kardianos/service"
	"github.com/spf13/cobra"
)

type commandService struct {
	status manager.Status
	err    error
	fail   error
	calls  []string
}

func (s *commandService) Status() (manager.Status, error) { return s.status, s.err }
func (s *commandService) Install() error {
	s.calls = append(s.calls, "install")
	if s.fail == nil {
		s.status, s.err = manager.StatusStopped, nil
	}
	return s.fail
}
func (s *commandService) Start() error {
	s.calls = append(s.calls, "start")
	s.status = manager.StatusRunning
	return nil
}
func (s *commandService) Stop() error {
	s.calls = append(s.calls, "stop")
	s.status = manager.StatusStopped
	return nil
}
func (s *commandService) Uninstall() error {
	s.calls = append(s.calls, "uninstall")
	s.err = manager.ErrNotInstalled
	return nil
}

func TestServiceInstallationWiresInstallThenStartWithAbsoluteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	svc := &commandService{err: manager.ErrNotInstalled}
	var output bytes.Buffer
	factory := func(cfg *manager.Config) (services.Controller, error) {
		if !reflect.DeepEqual(cfg.Arguments, []string{"service-run", "--config", path}) {
			t.Fatalf("service will not reuse enrollment: %v", cfg.Arguments)
		}
		return svc, nil
	}
	for range 2 {
		if err := controlService(path, &output, factory, "install", "start"); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(svc.calls, []string{"install", "start"}) || !strings.Contains(output.String(), "already running") {
		t.Fatalf("service installation was not idempotent: %v\n%s", svc.calls, output.String())
	}
}

func TestServiceInstallationFailureDoesNotStart(t *testing.T) {
	svc := &commandService{err: manager.ErrNotInstalled, fail: errors.New("installation failed")}
	var output bytes.Buffer
	err := controlService(filepath.Join(t.TempDir(), "config.yaml"), &output, func(*manager.Config) (services.Controller, error) { return svc, nil }, "install", "start")
	if !errors.Is(err, svc.fail) || !reflect.DeepEqual(svc.calls, []string{"install"}) {
		t.Fatalf("start after installation failure: %v: %v", svc.calls, err)
	}
}

func TestServiceControlCommandsAreRegistered(t *testing.T) {
	for _, action := range []string{"start", "stop", "uninstall", "service-run"} {
		command, _, err := rootCmd.Find([]string{action})
		if err != nil || command.Name() != action || command.Args == nil || command.RunE == nil {
			t.Fatalf("service command %s not wired: %v", action, err)
		}
	}
	if installCmd.Flags().Lookup("service-only") == nil {
		t.Fatal("no noninteractive installation for an enrolled agent")
	}
}

func TestServiceLogWriterDoesNotLoseWriteErrors(t *testing.T) {
	failure := errors.New("event log unavailable")
	writer := serviceLogWriter{log: func(...interface{}) error { return failure }}
	if n, err := writer.Write([]byte("service failed\n")); n != 0 || !errors.Is(err, failure) {
		t.Fatalf("logging failure lost: n=%d err=%v", n, err)
	}
}

func TestServiceOnlyInstallRequiresExistingEnrollment(t *testing.T) {
	previousPath, previousOnly := configPath, installServiceOnly
	configPath, installServiceOnly = filepath.Join(t.TempDir(), "missing-config.yaml"), true
	defer func() { configPath, installServiceOnly = previousPath, previousOnly }()
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	command.SetErr(&output)
	err := installCmd.RunE(command, nil)
	if err == nil || !strings.Contains(err.Error(), "saved enrollment") || output.Len() != 0 {
		t.Fatalf("service-only installation enrolled or claimed success: %v: %s", err, output.String())
	}
}
