package database

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type Tool struct {
	Name    string
	Path    string
	Version string
}

var clientVersion = regexp.MustCompile(`(?i)(?:Distrib|Ver|from)\s+([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)

func DetectTools(ctx context.Context, driver, version string) (Tool, Check) {
	return detectTools(ctx, driver, version, exec.LookPath, toolVersion)
}

func toolVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	command := exec.CommandContext(ctx, path, "--version")
	command.WaitDelay = time.Second
	var output cappedOutput
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()

	return output.String(), err
}

type cappedOutput struct{ buffer bytes.Buffer }

func (b *cappedOutput) Len() int       { return b.buffer.Len() }
func (b *cappedOutput) String() string { return b.buffer.String() }

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}

		b.buffer.Write(p)
	}

	return n, nil
}

func detectTools(ctx context.Context, driver, version string, lookup func(string) (string, error), run func(context.Context, string) (string, error)) (Tool, Check) {
	physical := []string{}
	for _, name := range []string{"mariabackup", "xtrabackup"} {
		if _, err := lookup(name); err == nil {
			physical = append(physical, name)
		}
	}

	note := ""
	if len(physical) > 0 {
		note = " Physical tools detected: " + strings.Join(physical, ", ") + "; their execution and filesystem access are not validated yet."
	}

	check := Check{Name: "Dump tooling"}
	found := false
	for _, name := range []string{"mariadb-dump", "mysqldump"} {
		path, err := lookup(name)
		if err != nil {
			continue
		}

		found = true
		output, err := run(ctx, path)
		if err != nil {
			continue
		}

		family := "mysql"
		if strings.Contains(strings.ToLower(output), "mariadb") {
			family = "mariadb"
		}

		matches := clientVersion.FindAllStringSubmatch(output, -1)
		if len(matches) == 0 {
			continue
		}

		// MariaDB prints a protocol version before the actual Distrib version.
		client := matches[len(matches)-1][1]
		major, minor := versionNumbers(client)
		serverMajor, serverMinor := versionNumbers(version)
		compatible := driver == family && major == serverMajor && minor >= serverMinor
		if !compatible {
			continue
		}

		check.Passed = true
		check.Message = fmt.Sprintf("Will use %s %s for a logical dump (--single-transaction --quick --routines --triggers --events --no-tablespaces).%s", name, client, note)

		return Tool{Name: name, Path: path, Version: client}, check
	}

	if driver == "" || version == "" {
		check.Message = "Cannot verify tool compatibility until the database version check passes." + note
	} else if found {
		check.Message = "Dump client could not run or has an unsupported version/family; install a client matching the database server version and check PATH." + note
	} else {
		check.Message = "No logical dump client on PATH. Install MySQL client utilities (mysqldump; mysql-client/mysql-community-client) or MariaDB client tools (mariadb-dump; mariadb-client), matching your server. On Windows add the installation's bin directory to the service/system PATH." + note
	}

	return Tool{}, check
}
