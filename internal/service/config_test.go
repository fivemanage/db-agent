package service

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestConfigUsesStablePathsAndAutomaticSystemService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config with spaces.yaml")
	cfg, err := configFor("db-agent", path, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.Executable) || !reflect.DeepEqual(cfg.Arguments, []string{"service-run", "--config", path}) {
		t.Fatalf("service depends on the installing shell's cwd: %+v", cfg)
	}
	if cfg.Name != Name || cfg.EnvVars["DB_AGENT_SERVICE_NAME"] != Name || cfg.Option["UserService"] != false {
		t.Fatal("unstable service identity or per-user installation")
	}
	if cfg.Option["StartType"] != "automatic" || cfg.Option["RunAtLoad"] != true {
		t.Fatal("service will not start after reboot")
	}
	if len(cfg.EnvVars) != 1 {
		t.Fatal("installation environment contains more than service identity")
	}
}

func TestConfigRejectsUnsafeServicePaths(t *testing.T) {
	paths := []string{"", "config\ncredential", "config\x00.yaml", "config\t.yaml"}
	if runtime.GOOS == "linux" {
		paths = append(paths, "config%h.yaml", "config$HOME.yaml", "config\".yaml", "config\\.yaml")
	}
	for _, path := range paths {
		if _, err := configFor("db-agent", path, runtime.GOOS); err == nil {
			t.Fatalf("unsafe service path accepted: %q", path)
		}
	}
}
