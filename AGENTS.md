# db-agent repository guide

## Project

This is the Go agent for FiveManage's customer-hosted MySQL and MariaDB backup feature. The agent runs on the customer's machine; FiveManage's control plane runs in the separate `fivemanage` repository. Windows Server is the primary installation target.

Read `SPEC.md` for the intended MVP behavior and `UPDATE.md` before changing the signed update protocol. Those documents describe planned work as well as implemented work. Currently, the CLI has configuration and signed update code, while enrollment, heartbeat, backups, and service installation are incomplete.

## Layout

| Path | Purpose |
| --- | --- |
| `main.go`, `cmd/` | CLI commands and process entry points |
| `internal/config/` | Local configuration, defaults, validation, and dashboard overrides |
| `internal/update/` | Signed manifest checks, downloads, binary replacement, and rollback |
| `tools/release-manifest/` | Offline release key and manifest tooling |
| `scripts/` | Release build scripts |
| `db/init/`, `docker-compose.yml` | Local development database fixtures |

## Working rules

- Load [code-style](.agents/skills/code-style/SKILL.md) when writing or reviewing Go code.
- Load [code-structure](.agents/skills/code-structure/SKILL.md) when adding a command, agent behavior, network integration, database access, or update flow.
- Keep the enrollment token separate from the issued agent credential. Never persist the enrollment token or send database credentials to the control plane.
- Treat local configuration as sensitive. Do not print credentials, encryption keys, or signed update private keys in logs or errors.
- Preserve signed update verification and rollback behavior when changing the updater. Release signing keys stay outside the repository.
- Add comments only when they explain a non-obvious constraint, invariant, security decision, or platform behavior. Do not narrate what the next line of code does.
- Preserve unrelated local changes. Release publishing and GitHub Actions setup are separate work.

## Checks

Run checks relevant to the files changed:

```sh
gofmt -w <changed-go-files>
go test ./...
go vet ./...
```

For platform-specific update code, also compile the affected target with `CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go build ./...`. The release targets are `windows/amd64`, `linux/amd64`, `linux/arm64`, and `darwin/arm64`. Cross-compilation does not replace a live service/update test on the target OS.
