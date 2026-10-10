# Signed agent updates

The agent checks `GET <api_url>/api/v1/agent/updates?os=<goos>&arch=<goarch>&current_version=<version>` when it starts and every 24 hours while `auto_update: true`. This release feed is public; the pinned signature authenticates it. You can instead set `update_url: "https://releases.example.com/db-agent/{os}-{arch}.json"` in the local config and host the signed JSON as static files. The endpoint returns one manifest for the latest stable release on that platform:

```json
{
  "version": "1.0.1",
  "os": "windows",
  "arch": "amd64",
  "url": "https://releases.example.com/db-agent/1.0.1/windows-amd64.exe",
  "sha256": "<lowercase SHA-256 of executable>",
  "signature": "<base64 Ed25519 signature>"
}
```

The release artifact URL must be stable HTTPS. Release signing uses an offline Ed25519 private key; the agent pins the matching public key at build time. The signature covers the version, OS, architecture, URL, and checksum. The agent refuses an invalid signature, wrong platform, non-HTTPS URL, checksum mismatch, or downgrade. The release feed cannot select a different signing key.

`db-agent update check` and `db-agent update now` can read a config containing only `update_url`; they do not need enrollment or database credentials. A standalone `update now` replaces the binary without launching an agent. Automatic checks run when `db-agent agent` or the installed Windows/systemd service starts with `auto_update: true`.

Automatic checks start after the initial heartbeat attempt and run in
an independent worker, so a slow release check/download cannot stall heartbeats.
A rejected initial agent credential stops the process without starting that
worker. A transient heartbeat outage does not disable the public signed feed.
Update handoff cancels and joins the heartbeat worker before the process exits;
manifest verification, replacement, and rollback behavior are unchanged.

## Release setup

Generate a signing key once. Keep the private file outside the repository and place it in a restricted CI secret. The command prints the base64 public key:

```sh
go run ./tools/release-manifest keygen --private /secure/path/db-agent-release.key
```

Build each static release with both a stable version and that public key embedded. The release script produces four executables and signed manifests:

```sh
./scripts/build-release.sh 1.0.1 <base64-public-key> /secure/path/db-agent-release.key https://releases.example.com/db-agent/1.0.1 ./dist
```

For a single target, the equivalent build is:

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-X github.com/fivemanage/db-agent/cmd.version=1.0.1 -X github.com/fivemanage/db-agent/cmd.updatePublicKey=<base64-public-key>" -o db-agent-windows-amd64.exe .
```

Publish the executable at a stable HTTPS URL, then sign its metadata:

```sh
go run ./tools/release-manifest sign --private /secure/path/db-agent-release.key --artifact db-agent-windows-amd64.exe --version 1.0.1 --os windows --arch amd64 --url https://releases.example.com/db-agent/1.0.1/windows-amd64.exe --output windows-amd64.json
```

The control plane may serve the signed JSON verbatim, or the files may be hosted alongside the public release binaries. Do not put the private key in the control plane or use expiring presigned artifact URLs, since changing the URL invalidates the signature. Build and sign separate manifests for `windows/amd64`, `linux/amd64`, `linux/arm64`, and `darwin/arm64`.

## Replacement and restart

The agent downloads a verified executable beside its installed binary. A separate copy of the old binary then waits for the agent to exit, replaces it, and restarts it. On Windows, replacement uses `ReplaceFileW` with a rollback copy. On Unix, the staged binary is renamed over the installed one with a rollback copy retained. If restart fails, the helper restores the old executable and tries to start it again. The rollback copy stays until the next successful update attempt.

The installed Windows/systemd service sets `DB_AGENT_SERVICE_NAME=db-agent`. The helper then uses `sc.exe` on Windows or `systemctl` on Linux. On Linux it launches in a transient systemd unit so stopping the agent unit does not kill the updater. Without that environment variable, the helper restarts `db-agent agent` as a foreground child. Launchd automatic updates are disabled: its helper cannot restart a service yet. Stop that service and update the binary manually instead.

The release endpoint, production signing key, release publishing, and live Windows/systemd update validation are still required before this can be called an unattended production update system. The current `dev` binary intentionally skips automatic checks.
