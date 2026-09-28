---
name: code-structure
description: Structure db-agent Go commands and internal packages for agent lifecycle, configuration, control-plane HTTP, backups, services, and signed updates. Use when adding or changing behavior or integrations.
---

# Go code structure

## Package boundaries

- Keep `main.go` limited to launching the CLI and reporting its final error.
- Keep `cmd/` focused on Cobra flags, prompts, output, and process lifecycle. Put reusable protocol, validation, and backup behavior in `internal/<domain>/` packages. Import from `cmd/` into `internal/`, never the reverse.
- Keep local config parsing, defaults, and validation in `internal/config/`. A dashboard override must only affect explicitly allowed fields; local database credentials and key paths stay local.
- Put enrollment and heartbeat HTTP handling in a focused internal package when implemented. Exchange a one-time enrollment token for an agent credential; use the issued credential for subsequent authenticated calls. Do not pass database credentials to the control plane.
- Separate database access and backup/restore orchestration from control-plane transport. Use narrow package APIs so commands can coordinate these operations without owning their mechanics.
- Keep manifest verification, download staging, replacement, and rollback in `internal/update/`. `tools/release-manifest/` and `scripts/` are release-side tooling; the running agent consumes only a pinned public key and signed metadata.
- Put OS-specific replacement and service behavior in platform-specific files where it genuinely differs. Keep cross-platform behavior shared.

## Before adding behavior

Read the relevant `SPEC.md` section for the intended contract and inspect nearby code for what exists today. The spec includes planned work. Update `SPEC.md` or `UPDATE.md` if a documented contract changes. Test the changed package and compile target platforms for OS-specific changes; a cross-build does not prove service behavior on Windows.
