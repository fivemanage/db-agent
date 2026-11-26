package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"unicode"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
)

type dumpToolSetup struct {
	inspect     func(context.Context, config.DatabaseConfig) database.Report
	detect      func(context.Context, string, string) (database.Tool, database.Check)
	discover    func(context.Context, string, string) []database.DiscoveredTool
	addToPath   func(string) error
	planInstall func(string) (database.ToolInstallPlan, bool)
	installTool func(context.Context, database.ToolInstallPlan) error
	isWindows   bool
}

func defaultDumpToolSetup() dumpToolSetup {
	return dumpToolSetup{
		inspect:     database.Inspect,
		detect:      database.DetectTools,
		discover:    database.DiscoverOffPathTools,
		addToPath:   database.AddToSystemPath,
		planInstall: database.PlanDumpToolInstall,
		installTool: database.InstallDumpTool,
		isWindows:   runtime.GOOS == "windows",
	}
}

func ensureInstallDumpTool(ctx context.Context, input *bufio.Reader, output io.Writer, db config.DatabaseConfig) {
	ensureDumpTool(ctx, input, output, db, defaultDumpToolSetup())
}

// ensureDumpTool verifies a compatible logical dump client exists right after
// database setup. Missing or off-PATH tools are surfaced and fixed in-place
// when permitted, without discarding saved enrollment or database settings.
func ensureDumpTool(ctx context.Context, input *bufio.Reader, output io.Writer, db config.DatabaseConfig, setup dumpToolSetup) {
	if ctx.Err() != nil {
		return
	}

	view := newSetupOutput(output)
	view.section("Backup tool")

	stop := view.spin("Checking for a matching dump client")
	report := setup.inspect(ctx, db)
	stop()
	if ctx.Err() != nil {
		return
	}

	driver, version := report.Driver, report.Version
	if driver == "" || version == "" {
		view.warn("Couldn't detect the database server version to verify dump tools. Run db-agent test after setup.")
		return
	}

	tool, check := setup.detect(ctx, driver, version)
	if check.Passed {
		view.ok(fmt.Sprintf("Found %s %s", displayText(tool.Name), displayText(tool.Version)))
		return
	}

	for {
		if ctx.Err() != nil {
			return
		}

		resolved, foundOffPath := tryFixOffPathTool(ctx, input, output, driver, version, setup)
		if resolved {
			return
		}

		if !foundOffPath {
			if plan, ok := setup.planInstall(driver); ok {
				if tryPackageInstallTool(ctx, input, output, driver, version, plan, setup) {
					return
				}
				view.note("Install a matching dump client, then run db-agent test or retry from the dashboard.")
				return
			}

			printManualDumpToolGuidance(view, driver, version, setup.isWindows)
		}

		retry, err := confirm(input, output, "Check again after fixing?", false)
		if err != nil || !retry {
			view.note("Continuing setup. Make sure a matching dump client is available before finishing in the dashboard.")
			return
		}

		stop = view.spin("Re-checking for a matching dump client")
		tool, check = setup.detect(ctx, driver, version)
		stop()
		if check.Passed {
			view.ok(fmt.Sprintf("Found %s %s", displayText(tool.Name), displayText(tool.Version)))
			return
		}
	}
}

func tryFixOffPathTool(ctx context.Context, input *bufio.Reader, output io.Writer, driver, version string, setup dumpToolSetup) (resolved bool, found bool) {
	view := newSetupOutput(output)
	discovered := setup.discover(ctx, driver, version)
	if len(discovered) == 0 {
		return false, false
	}

	var trusted *database.DiscoveredTool
	var untrusted *database.DiscoveredTool
	for i := range discovered {
		if discovered[i].Trusted && trusted == nil {
			trusted = &discovered[i]
		} else if !discovered[i].Trusted && untrusted == nil {
			untrusted = &discovered[i]
		}
	}

	if trusted != nil {
		dirText := displayPath(trusted.Dir)
		view.info(fmt.Sprintf("Found compatible %s %s at %s (not on PATH).", displayText(trusted.Tool.Name), displayText(trusted.Tool.Version), dirText))
		add, err := confirm(input, output, "Add this folder to System PATH?", true)
		if err != nil || !add {
			view.note(fmt.Sprintf("Add %s to your System PATH manually, then run db-agent test.", dirText))
			return false, true
		}

		if err := setup.addToPath(trusted.Dir); err != nil {
			if errors.Is(err, database.ErrSystemPathUnsupported) {
				view.warn(fmt.Sprintf("Automatic System PATH updates aren't supported here. Make %s available on the service PATH (or symlink it into /usr/local/bin).", displayPath(trusted.Tool.Path)))
			} else {
				view.warn(fmt.Sprintf("Couldn't add %s to System PATH automatically: %v", dirText, err))
				view.note(fmt.Sprintf("Add %s to your System PATH manually and restart the db-agent service.", dirText))
			}
			return false, true
		}

		tool, check := setup.detect(ctx, driver, version)
		if check.Passed {
			view.ok(fmt.Sprintf("Added %s to System PATH and verified %s %s", dirText, displayText(tool.Name), displayText(tool.Version)))
			return true, true
		}

		view.warn(fmt.Sprintf("Added %s to System PATH, but %s still wasn't verified.", dirText, displayText(trusted.Tool.Name)))
		view.note("Restart the db-agent service and run db-agent test.")
		return false, true
	}

	if untrusted != nil {
		view.warn(fmt.Sprintf("Found %s in %s, but that folder is writable by non-administrators so the service won't trust it automatically.", displayText(untrusted.Tool.Name), displayPath(untrusted.Dir)))
		if setup.isWindows {
			view.note("Install official " + serverName(driver) + " client tools under Program Files (or restrict that folder to Administrators/SYSTEM), then add its bin folder to System PATH.")
		} else {
			view.note("Install official " + serverName(driver) + " client tools into a protected system directory (such as /usr/bin) or remove world-write permissions.")
		}
		return false, true
	}

	return false, false
}

// displayPath preserves Windows backslashes while escaping control characters.
func displayPath(path string) string {
	if strings.ContainsFunc(path, unicode.IsControl) {
		return displayText(path)
	}
	return path
}

func tryPackageInstallTool(ctx context.Context, input *bufio.Reader, output io.Writer, driver, version string, plan database.ToolInstallPlan, setup dumpToolSetup) bool {
	view := newSetupOutput(output)
	view.warn(fmt.Sprintf("No compatible %s dump client found on PATH (server is %s %s).", serverName(driver), serverName(driver), displayText(version)))
	view.note("Package command: " + displayText(plan.Summary))
	install, err := confirm(input, output, fmt.Sprintf("Install %s client tools now?", serverName(driver)), true)
	if err != nil || !install {
		view.note("Skipped automatic installation. Run `" + displayText(plan.Summary) + "` manually before finishing setup.")
		return false
	}

	stop := view.spin(fmt.Sprintf("Installing %s client tools (%s)", serverName(driver), displayText(plan.Package)))
	installErr := setup.installTool(ctx, plan)
	stop()
	if installErr != nil {
		view.warn(sentence(installErr.Error()))
		return false
	}

	tool, check := setup.detect(ctx, driver, version)
	if check.Passed {
		view.ok(fmt.Sprintf("Installed and verified %s %s", displayText(tool.Name), displayText(tool.Version)))
		return true
	}

	view.warn(fmt.Sprintf("Installed %s, but no compatible client for %s %s was detected on PATH.", displayText(plan.Package), serverName(driver), displayText(version)))
	view.note(check.Message)
	return false
}

func printManualDumpToolGuidance(view setupOutput, driver, version string, isWindows bool) {
	toolName := "mariadb-dump"
	if driver == "mysql" {
		toolName = "mysqldump"
	}
	view.warn(fmt.Sprintf("No compatible %s dump client found on PATH (need %s matching %s %s or newer in the same major version).", serverName(driver), toolName, serverName(driver), displayText(version)))
	if isWindows {
		view.note(fmt.Sprintf("Install %s client tools matching %s, add its bin folder to System PATH, then re-check here (or restart the db-agent service).", serverName(driver), displayText(version)))
		return
	}
	if driver == "mysql" {
		view.note("Install MySQL client tools (mysql-client / mysql-community-client) matching your server version.")
		return
	}
	view.note("Install MariaDB client tools (mariadb-client) matching your server version.")
}
