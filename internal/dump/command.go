// Package dump executes scoped logical backups without plaintext dump files.
package dump

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/database"
	"github.com/fivemanage/db-agent/internal/securefile"
)

type command struct {
	path string
	args []string
	env  []string
}

func validate(settings config.DatabaseConfig, tool database.Tool, options Options) error {
	if err := database.ValidateLocalConnection(settings); err != nil {
		return fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	if strings.ContainsRune(settings.User, 0) || strings.ContainsRune(settings.Password, 0) {
		return ErrConfiguration
	}
	seen := make(map[string]bool)
	for _, name := range settings.Databases {
		// Reject option-shaped identifiers as well as inserting the option terminator.
		if strings.HasPrefix(name, "-") || seen[name] {
			return ErrConfiguration
		}
		seen[name] = true
	}
	if (tool.Name != "mysqldump" && tool.Name != "mariadb-dump") || tool.Path == "" || !filepath.IsAbs(tool.Path) || tool.Version == "" {
		return ErrTool
	}
	if options.Timeout < 0 || options.BytesPerSecond < 0 {
		return ErrConfiguration
	}
	return nil
}

func buildCommand(settings config.DatabaseConfig, tool database.Tool, optionsFile, privateDir string) command {
	args := []string{
		"--defaults-file=" + optionsFile,
		"--single-transaction", "--quick", "--routines", "--triggers", "--events",
		"--no-tablespaces", "--skip-lock-tables", "--hex-blob", "--default-character-set=utf8mb4",
	}
	if settings.Driver == "mysql" {
		// Scoped logical backups must not purge global GTID state or need RELOAD grants.
		args = append(args, "--set-gtid-purged=OFF")
		major, _ := strconv.Atoi(strings.Split(tool.Version, ".")[0])
		if major >= 8 {
			args = append(args, "--column-statistics=0", "--get-server-public-key")
		}
	}
	args = append(args, "--databases", "--")
	args = append(args, settings.Databases...)
	return command{path: tool.Path, args: args, env: clientEnvironment(os.Environ(), privateDir)}
}

func clientEnvironment(env []string, privateDir string) []string {
	clean := make([]string, 0, len(env)+1)
	for _, value := range env {
		name, _, _ := strings.Cut(value, "=")
		name = strings.ToUpper(name)
		if !strings.HasPrefix(name, "MYSQL_") && !strings.HasPrefix(name, "MARIADB_") {
			clean = append(clean, value)
		}
	}
	// Even --defaults-file reads MySQL's login file. Redirect it to a nonexistent
	// path inside our private directory (supported by MySQL 5.7 and later).
	return append(clean, "MYSQL_TEST_LOGIN_FILE="+filepath.Join(privateDir, "no-login.cnf"))
}

func quoteOption(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\b", `\b`).Replace(value)
	return `"` + value + `"`
}

func createOptions(settings config.DatabaseConfig, parent string) (string, string, error) {
	dir, err := os.MkdirTemp(parent, ".db-agent-dump-")
	if err != nil {
		return "", "", fileFailure(err)
	}
	if err := protectDirectory(dir); err != nil {
		cleanupErr := os.Remove(dir)
		if cleanupErr != nil {
			return "", "", errors.Join(fileFailure(err), ErrCleanup)
		}
		return "", "", fileFailure(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		if cleanupErr := os.Remove(dir); cleanupErr != nil {
			return "", "", errors.Join(ErrCredentials, ErrCleanup)
		}
		return "", "", ErrCredentials
	}
	dir = abs
	path := filepath.Join(dir, "client.cnf")
	host := settings.Host
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	} else {
		host = net.ParseIP(host).String()
	}
	contents := []byte(fmt.Sprintf("[client]\nuser=%s\npassword=%s\nhost=%s\nport=%d\nprotocol=tcp\n", quoteOption(settings.User), quoteOption(settings.Password), quoteOption(host), settings.Port))
	defer clear(contents)
	file, err := securefile.OpenNew(path)
	if err == nil {
		_, err = file.Write(contents)
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
	}
	if err != nil {
		cleanupErr := cleanupOptions(path, dir)
		return "", "", errors.Join(fileFailure(err), cleanupErr)
	}
	return path, dir, nil
}

func cleanupOptions(path, dir string) error {
	fileErr := os.Remove(path)
	if errors.Is(fileErr, os.ErrNotExist) {
		fileErr = nil
	}
	dirErr := os.Remove(dir)
	if fileErr != nil || dirErr != nil {
		return ErrCleanup
	}
	return nil
}
