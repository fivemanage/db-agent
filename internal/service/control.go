package service

import (
	"errors"
	"fmt"
	"os"

	manager "github.com/kardianos/service"
)

type Controller interface {
	Status() (manager.Status, error)
	Install() error
	Uninstall() error
	Start() error
	Stop() error
}

func NewController(cfg *manager.Config) (Controller, error) {
	if err := requireAdministrator(); err != nil {
		return nil, err
	}
	svc, err := New(nil, cfg)
	if err != nil {
		return nil, err
	}
	return platformController(svc), nil
}

// Control only removes the service registration. It never opens agent data files.
func Control(svc Controller, action string) (string, error) {
	switch action {
	case "install", "start", "stop", "uninstall":
	default:
		return "", fmt.Errorf("unknown service action %q", action)
	}
	status, err := svc.Status()
	missing := errors.Is(err, manager.ErrNotInstalled)
	if err != nil && !missing {
		return "", controlError("check", err)
	}
	if !missing && status != manager.StatusRunning && status != manager.StatusStopped {
		return "", errors.New("cannot determine service state; inspect the OS service manager before retrying")
	}
	var operation func() error
	var message string
	switch action {
	case "install":
		if !missing {
			return "Service already installed.", nil
		}
		operation, message = svc.Install, "Service installed (starts automatically after reboot)."
	case "start":
		if missing {
			return "", errors.New("service is not installed; run db-agent install --service-only with the enrolled config first")
		}
		if status == manager.StatusRunning {
			return "Service already running.", nil
		}
		operation, message = svc.Start, "Service is starting."
	case "stop":
		if missing {
			return "Service not installed; nothing to stop.", nil
		}
		if status == manager.StatusStopped {
			return "Service already stopped.", nil
		}
		operation, message = svc.Stop, "Service stopped."
	case "uninstall":
		if missing {
			return "Service already uninstalled. Config, keys, backups, and logs preserved.", nil
		}
		if status == manager.StatusRunning {
			if err := svc.Stop(); err != nil {
				return "", controlError("stop before uninstalling", err)
			}
		}
		operation, message = svc.Uninstall, "Service uninstalled. Config, keys, backups, and logs preserved."
	}
	if err := operation(); err != nil {
		return "", controlError(action, err)
	}
	return message, nil
}

func controlError(action string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("permission denied trying to %s the service; run an elevated Administrator terminal on Windows or use sudo on Linux: %w", action, err)
	}
	return fmt.Errorf("could not %s the service; check the OS service manager and Administrator/root permissions: %w", action, err)
}
