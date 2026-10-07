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

func systemdStatus(output string) (manager.Status, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			fields[key] = value
		}
	}
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
