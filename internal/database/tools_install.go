package database

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const toolInstallTimeout = 3 * time.Minute

// ToolInstallPlan describes a package-manager installation of a matching
// logical dump client on Linux.
type ToolInstallPlan struct {
	Manager  string
	Package  string
	Summary  string
	Commands [][]string
}

// PlanDumpToolInstall returns a safe, explicit package-manager command plan for
// installing a logical dump client matching driver on Linux, or false if no
// supported package manager is available.
func PlanDumpToolInstall(driver string) (ToolInstallPlan, bool) {
	if runtime.GOOS != "linux" {
		return ToolInstallPlan{}, false
	}
	return planDumpToolInstall(driver, exec.LookPath)
}

func planDumpToolInstall(driver string, lookPath func(string) (string, error)) (ToolInstallPlan, bool) {
	if driver != "mariadb" && driver != "mysql" {
		return ToolInstallPlan{}, false
	}

	if aptPath, err := lookPath("apt-get"); err == nil {
		pkg := "mariadb-client"
		if driver == "mysql" {
			pkg = "mysql-client"
		}
		return ToolInstallPlan{
			Manager: "apt-get",
			Package: pkg,
			Summary: fmt.Sprintf("apt-get update && apt-get install -y %s", pkg),
			Commands: [][]string{
				{aptPath, "update"},
				{aptPath, "install", "-y", pkg},
			},
		}, true
	}

	if dnfPath, err := lookPath("dnf"); err == nil {
		pkg := "mariadb"
		if driver == "mysql" {
			pkg = "mysql"
		}
		return ToolInstallPlan{
			Manager: "dnf",
			Package: pkg,
			Summary: fmt.Sprintf("dnf install -y %s", pkg),
			Commands: [][]string{
				{dnfPath, "install", "-y", pkg},
			},
		}, true
	}

	if yumPath, err := lookPath("yum"); err == nil {
		pkg := "mariadb"
		if driver == "mysql" {
			pkg = "mysql"
		}
		return ToolInstallPlan{
			Manager: "yum",
			Package: pkg,
			Summary: fmt.Sprintf("yum install -y %s", pkg),
			Commands: [][]string{
				{yumPath, "install", "-y", pkg},
			},
		}, true
	}

	if apkPath, err := lookPath("apk"); err == nil {
		pkg := "mariadb-client"
		if driver == "mysql" {
			pkg = "mysql-client"
		}
		return ToolInstallPlan{
			Manager: "apk",
			Package: pkg,
			Summary: fmt.Sprintf("apk add --no-cache %s", pkg),
			Commands: [][]string{
				{apkPath, "add", "--no-cache", pkg},
			},
		}, true
	}

	return ToolInstallPlan{}, false
}

// InstallDumpTool executes an approved ToolInstallPlan non-interactively with a
// bounded timeout. Raw package-manager output is suppressed from returned errors.
func InstallDumpTool(ctx context.Context, plan ToolInstallPlan) error {
	if len(plan.Commands) == 0 {
		return errors.New("no dump client install commands configured")
	}

	runCtx, cancel := context.WithTimeout(ctx, toolInstallTimeout)
	defer cancel()

	for _, argv := range plan.Commands {
		if len(argv) == 0 {
			continue
		}
		cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
		cmd.WaitDelay = 5 * time.Second
		if plan.Manager == "apt-get" {
			cmd.Env = append(cmd.Environ(), "DEBIAN_FRONTEND=noninteractive")
		}
		var out cappedOutput
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			if runCtx.Err() != nil {
				return fmt.Errorf("installing %s timed out or was canceled", plan.Package)
			}
			return fmt.Errorf("%s failed while installing %s; run `%s` manually to inspect package output", plan.Manager, plan.Package, strings.TrimSpace(plan.Summary))
		}
	}

	return nil
}
