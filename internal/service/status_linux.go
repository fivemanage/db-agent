package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	manager "github.com/kardianos/service"
)

type systemdController struct{ manager.Service }

func platformController(svc manager.Service) Controller { return systemdController{svc} }

func (s systemdController) Status() (manager.Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", "show", Name+".service", "--property=LoadState,ActiveState").Output()
	if err != nil {
		if _, stateErr := systemdStatus(string(output)); errors.Is(stateErr, manager.ErrNotInstalled) {
			return manager.StatusUnknown, manager.ErrNotInstalled
		}
		return manager.StatusUnknown, fmt.Errorf("query systemd service: %w", err)
	}
	return systemdStatus(string(output))
}

func running(Controller) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", "show", Name+".service", "--property=ActiveState,SubState").Output()
	if err != nil {
		return false, fmt.Errorf("query systemd service: %w", err)
	}
	return systemdRunning(string(output)), nil
}

// systemdRunning excludes "activating" states, unlike systemdStatus: a
// crashing service waits in activating/auto-restart between restart attempts.
func systemdRunning(output string) bool {
	fields := systemdFields(output)
	return fields["ActiveState"] == "active" && fields["SubState"] == "running"
}

func systemdFields(output string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			fields[key] = value
		}
	}
	return fields
}

func systemdStatus(output string) (manager.Status, error) {
	fields := systemdFields(output)
	if fields["LoadState"] == "not-found" {
		return manager.StatusUnknown, manager.ErrNotInstalled
	}
	if fields["LoadState"] != "loaded" {
		return manager.StatusUnknown, errors.New("systemd service is masked or could not be loaded; inspect systemctl status db-agent")
	}
	// kardianos reports a failed unit as an unknown state; it is still installed
	// and must remain startable/uninstallable without reinstalling its definition.
	switch fields["ActiveState"] {
	case "active", "activating", "reloading", "deactivating":
		return manager.StatusRunning, nil
	case "inactive", "failed":
		return manager.StatusStopped, nil
	default:
		return manager.StatusUnknown, errors.New("systemd returned an unknown service state")
	}
}
