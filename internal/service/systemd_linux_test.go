package service

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	manager "github.com/kardianos/service"
)

func TestLinuxUnitKeepsPrivateFilesAndUpdateHandoffUsable(t *testing.T) {
	cfg, err := configFor("/opt/db-agent/db-agent", "/etc/db-agent/config.yaml", "linux")
	if err != nil {
		t.Fatal(err)
	}
	script, ok := cfg.Option["SystemdScript"].(string)
	if !ok {
		t.Fatal("systemd installation does not use the restrictive unit")
	}
	unit, err := template.New("systemd").Funcs(template.FuncMap{
		"cmdEscape": func(value string) string { return value },
		"cmd":       func(value string) string { return `"` + value + `"` },
	}).Parse(script)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	data := struct {
		*manager.Config
		Path string
	}{cfg, filepath.ToSlash(cfg.Executable)}
	if err := unit.Execute(&output, data); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"UMask=0077", "NoNewPrivileges=yes", "KillMode=control-group", "TimeoutStopSec=20",
		"User=root", "WorkingDirectory=/", "StandardOutput=journal", "StandardError=journal",
		"Restart=on-failure", "WantedBy=multi-user.target", "DB_AGENT_SERVICE_NAME=db-agent",
		`service-run" "--config" "/etc/db-agent/config.yaml"`,
	} {
		if !strings.Contains(output.String(), required) {
			t.Errorf("generated unit is missing %q", required)
		}
	}
	for _, incompatible := range []string{"PrivateTmp=", "ProtectSystem=", "EnvironmentFile="} {
		if strings.Contains(output.String(), incompatible) {
			t.Errorf("unit may hide configured state or inherit uncontrolled service settings: %s", incompatible)
		}
	}
}
