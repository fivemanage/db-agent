package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// ErrSystemPathUnsupported is returned when automatic system PATH updates are
// not supported on the current operating system.
var ErrSystemPathUnsupported = errors.New("automatic system PATH updates are not supported on this platform")

// DiscoveredTool represents a dump binary found outside the active search PATH.
// Untrusted binaries are never executed to read their version.
type DiscoveredTool struct {
	Tool    Tool
	Dir     string
	Trusted bool
}

type candidateBinary struct {
	name    string
	path    string
	dir     string
	trusted bool
}

// DiscoverOffPathTools searches standard installation directories for a dump
// client matching driver and version. Trusted binaries are version-checked;
// untrusted binaries are reported without execution so setup can explain why.
func DiscoverOffPathTools(ctx context.Context, driver, version string) []DiscoveredTool {
	return discoverOffPathTools(ctx, driver, version, findOffPathCandidates, toolVersion)
}

func discoverOffPathTools(
	ctx context.Context,
	driver, version string,
	findCandidates func([]string) []candidateBinary,
	run func(context.Context, string) (string, error),
) []DiscoveredTool {
	names := preferredDumpToolNames(driver)
	candidates := findCandidates(names)
	var discovered []DiscoveredTool
	seenPaths := make(map[string]struct{}, len(candidates))

	for _, candidate := range candidates {
		clean := filepath.Clean(candidate.path)
		key := strings.ToLower(clean)
		if _, ok := seenPaths[key]; ok {
			continue
		}
		seenPaths[key] = struct{}{}

		if !candidate.trusted {
			discovered = append(discovered, DiscoveredTool{
				Tool:    Tool{Name: candidate.name, Path: clean},
				Dir:     filepath.Clean(candidate.dir),
				Trusted: false,
			})
			continue
		}

		output, err := run(ctx, clean)
		if err != nil {
			continue
		}
		clientVersion, ok := matchToolVersion(driver, version, output)
		if !ok {
			continue
		}

		discovered = append(discovered, DiscoveredTool{
			Tool: Tool{
				Name:    candidate.name,
				Path:    clean,
				Version: clientVersion,
			},
			Dir:     filepath.Clean(candidate.dir),
			Trusted: true,
		})
	}

	return discovered
}

func preferredDumpToolNames(driver string) []string {
	if driver == "mysql" {
		return []string{"mysqldump", "mariadb-dump"}
	}
	return []string{"mariadb-dump", "mysqldump"}
}

// appendWindowsPathEntry appends dir to a raw semicolon-delimited Windows PATH
// string without expanding existing %VAR% placeholders or duplicating entries.
func appendWindowsPathEntry(raw, dir string) (string, bool, error) {
	cleanDir := normalizeWindowsPathDir(dir)
	if !isLocalWindowsAbsPath(cleanDir) {
		return "", false, errors.New("system PATH entry must be a local drive-qualified directory")
	}

	targetKey := strings.ToLower(cleanDir)
	for _, entry := range strings.Split(raw, ";") {
		trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(entry), `"`))
		if trimmed == "" {
			continue
		}
		if strings.ToLower(normalizeWindowsPathDir(trimmed)) == targetKey {
			return raw, false, nil
		}
	}

	trimmedRaw := strings.TrimRight(raw, "; ")
	if trimmedRaw == "" {
		return cleanDir, true, nil
	}

	return trimmedRaw + ";" + cleanDir, true, nil
}

func normalizeWindowsPathDir(dir string) string {
	normalized := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(dir), "/", `\`), `\`)
	if len(normalized) == 2 && normalized[1] == ':' {
		normalized += `\`
	}
	return normalized
}
