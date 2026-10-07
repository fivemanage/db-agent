package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	manager "github.com/kardianos/service"
)

const Name = "db-agent"

func Config(path string) (*manager.Config, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find service executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve service executable: %w", err)
	}
	return configFor(executable, path, runtime.GOOS)
}

func configFor(executable, path, goos string) (*manager.Config, error) {
	if path == "" {
		return nil, errors.New("service config path is required")
	}
	var err error
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve service config path: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve service executable path: %w", err)
	}
	for _, value := range []string{executable, path} {
		if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return nil, errors.New("service paths cannot contain control characters")
		}
		// The library's systemd template does not escape these expansions.
		if goos == "linux" && strings.ContainsAny(value, "%$\"\\") {
			return nil, errors.New("systemd service paths cannot contain %, $, quotes, or backslashes; move the executable/config to a plain path")
		}
	}
	cfg := &manager.Config{
		Name:        Name,
		DisplayName: "FiveManage Database Agent",
		Description: "FiveManage customer-hosted MySQL and MariaDB backup agent",
		Executable:  executable,
		Arguments:   []string{"service-run", "--config", path},
		EnvVars:     map[string]string{"DB_AGENT_SERVICE_NAME": Name},
		Option: manager.KeyValue{
			"StartType":        "automatic",
			"DelayedAutoStart": true,
			"Restart":          "on-failure",
			"RunAtLoad":        true,
			"KeepAlive":        false,
			"UserService":      false,
		},
	}
	if goos == "linux" {
		cfg.Dependencies = []string{"Wants=network-online.target", "After=network-online.target"}
	}
	return cfg, nil
}

func New(program manager.Interface, cfg *manager.Config) (manager.Service, error) {
	svc, err := manager.New(program, cfg)
	if err != nil {
		return nil, fmt.Errorf("service manager unavailable; use Windows Service, Linux systemd, or macOS launchd: %w", err)
	}
	switch svc.Platform() {
	case "windows-service", "linux-systemd", "darwin-launchd":
		return svc, nil
	default:
		return nil, fmt.Errorf("unsupported service manager %q; Linux requires systemd", svc.Platform())
	}
}
