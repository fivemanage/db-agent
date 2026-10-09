# db-agent

Customer-hosted MySQL/MariaDB backup agent for FiveManage. **Work in progress:**
registration, local database setup, recovery-key generation, diagnostics,
foreground/service heartbeats, and signed update support exist. Scheduled backups
and uploads are not implemented yet. A passing diagnostic or running service does
not mean a backup exists.

The streaming backup codec is implemented in `internal/artifact`: zstd (default)
or gzip, locally encrypted with chunked AES-256-GCM, with encrypted size/SHA-256
metadata and restore decoding. The CLI does not run this pipeline yet. See
[ARTIFACT.md](ARTIFACT.md) for the versioned format, security constraints, and
future upload/restore integration.

The scoped logical dump executor is implemented in `internal/dump`, with private
client credentials, timeouts, process-tree cancellation, and output throttling.
It is not yet wired into `backup --now`. See [DUMP.md](DUMP.md) for its contract,
consistency limits, and real MySQL/MariaDB integration tests.

## Register and connect

```sh
go build -o db-agent .
./db-agent install --api-url https://your-control-plane.example --config ./config.yaml
./db-agent test --config ./config.yaml
```

On Windows use `db-agent.exe`. Setup and diagnostics should run as the intended
service account (Administrator on Windows for the private config). The setup
commands also run on Linux. The shared runtime supports Windows Service, Linux
systemd, and macOS launchd; Linux distribution/installation validation remains
DEV-508 work.

`install` saves the issued agent credential **before** database setup. Rerunning
it resumes without another one-time token and preserves existing database
settings and key material. Use `db-agent configure --config ./config.yaml` to
retry failed database setup or explicitly change an existing connection.

The wizard offers a hidden paste of the game server's connection-string value,
or manual host/port/username/password/database input. Supported paste forms:

```text
mysql://user:password@127.0.0.1:3306/game_database
server=127.0.0.1;port=3306;database=game_database;userid=user;password=secret
```

URI special characters must be percent-encoded. Optional `charset=utf8mb4` is
accepted on URIs. Unsupported options (including TLS/socket settings) and
duplicate fields are rejected, not silently dropped. No server files are scanned.
Setup accepts only localhost/loopback connections, shows non-secret fields for
confirmation, and tests the selected application database before changing SQL
or saving database settings. Nothing is implicitly selected.

If the supplied account has direct `CREATE USER` permission, the wizard offers
to create `db_agent_backup@localhost` with a cryptographically generated password
after showing the SQL and requiring explicit approval. A second identity for the
selected loopback IP (`127.0.0.1` or `::1`) receives the same password/grants:
Linux MariaDB and MySQL with name resolution disabled do not necessarily match
local TCP to `@localhost`. `localhost` input is pinned to IPv4 TCP. No remote-host
or wildcard-host identity is created. It does **not** alter an existing account
or reset its password. Schema grants escape wildcard characters
to restrict access to the confirmed database. The new account is tested before
saving; grant, verification, or config-save failure removes only the identities
confirmed created by that attempt. If cleanup fails, ask a local administrator
to inspect and remove those new identities before retrying. An ambiguous CREATE
failure never triggers automatic removal of a possibly pre-existing account.

If creation is declined or unavailable, supply an existing backup account with
the displayed schema/routine grants. Root and accounts with detected admin or
grant-authority privileges cannot be saved by the wizard. Admin credentials are
ephemeral; only a tested, confirmed backup account is persisted. The wizard
offers a retry after failed steps; enrollment does not need to be repeated.

Changes replace the config privately while retaining other settings. Database
credentials stay on the local machine and are never included in the API probe.
Alternatively edit `database` in the private config using the example in `SPEC.md`.
No database is implicitly selected when `databases` is empty.

Release builds can set `cmd.defaultAPIURL` via Go linker flags to avoid asking
users for an API URL. Until a production endpoint is configured, development
builds require `--api-url` for initial enrollment; resumed setup uses the saved
URL. `install` sets up the recovery key after database setup, runs diagnostics, and installs
and starts the service only if all prerequisites pass. Failed checks leave any
existing service unchanged. The first backup remains incomplete,
so the wizard still returns a nonzero incomplete-setup result rather than claiming
a working backup installation. Saved settings remain available for the next attempt.

Connection and discovery use a ten-second deadline. MySQL 5.7+ and MariaDB 10.3+
are the initial supported server versions. Diagnostics print PASS/FAIL/SKIP for
config, connectivity, authentication, version, selection, grants, dump tooling,
encryption key access, and API reachability, and return nonzero if anything fails
or is skipped. Agent startup prints the same prerequisites.

### Recovery key setup (DEV-487)

Normal `install` generates the recovery key after DB configuration. To set up or
resume only the key for an already-enrolled agent:

```sh
./db-agent configure --key --config ./config.yaml
```

Run interactively with unredirected stdout. **Do not record the terminal, use
PowerShell transcripts, or pipe this command into a log collector.** The only
key display goes directly to the terminal, not to command/log writers. Paste the
saved key back through hidden input, then type `SAVED` to acknowledge that a copy
is stored safely outside this machine. Keep it in an offline/password-manager
location, not a support ticket. We never receive or retain a copy.

The key file defaults to `encryption.key` beside the selected config. Relative
configured key paths are resolved against the config directory during key setup
and saved as absolute paths. Each key is 32 bytes from Go's cryptographic RNG.
Its portable recovery/file format is one ASCII line: `dbak1_` followed by the
43-character, unpadded base64url encoding of those 32 bytes, with a trailing
newline in the file. The prefix versions key encoding only; the separate backup
artifact format is documented in [ARTIFACT.md](ARTIFACT.md). Arbitrary old
placeholder files are not valid keys and are never automatically overwritten.

Files are staged privately, synced, and published without replacing an existing
destination. Config/pending-enrollment files use the same safe creation primitive;
config changes are private atomic replacements preserving unrelated fields.
Unix files use `0600`; Windows files use protected System/Administrators-only
ACLs. Config and key readers reject symlinks, nonregular/oversized files, and broad
permissions. Manually created configs must be restricted before use (`chmod 600`
on Unix; a protected System/Administrators-only ACL on Windows).

Setup saves only the absolute key path, SHA-256 key fingerprint (`key_id`), and
`key_acknowledged` in config. The key itself lives only in its private file and
the deliberate terminal display. Do not manually invent/change acknowledgement
or fingerprint fields. Diagnostics verify the format, permissions, fingerprint,
and acknowledgement; an unreadable, changed, missing, or unacknowledged key is
not backup-ready.

**A retry never redisplays or regenerates an existing key.** If setup crashes,
output fails, or confirmation is declined—even before the first display finishes—
the saved key remains in place. Rerun `configure --key` and confirm using your saved
copy. If you never received that display, an authorized local administrator must
copy the protected key file to safe offline storage and use that copy for hidden
confirmation. Do not delete the key or request a fresh enrollment token. A
configured missing key must be restored from its original recovery copy; setup
will not replace it. Uninstall preserves it and its acknowledgement.

### Run the foreground agent

```sh
./db-agent agent --config ./config.yaml
```

This is a foreground process on both Windows and Linux, **not an installed
service**. After startup diagnostics, it sends an immediate authenticated
heartbeat to `/v1/agent/heartbeat`, then polls every 60 seconds. It uses only the
issued `dac_` credential, agent version, activity, classified error messages, and
optional discovered DB driver/numeric version.
Database credentials, database names, key material/paths, and raw diagnostic
messages are never included. Incomplete local prerequisites report `error` with
a generic setup message rather than falsely reporting backup readiness. The
unauthenticated base-URL reachability probe does not override local readiness;
actual connectivity is determined by the authenticated heartbeat response.

The client uses a 15-second request timeout and a 45-second total budget per
heartbeat, with up to three attempts and jittered exponential backoff. It retries
transport failures, HTTP 429, and HTTP 5xx. `Retry-After` is honored across polls,
even when its delay exceeds one request budget. Other HTTP failures, redirects,
and malformed responses stop the agent with an actionable error. HTTP 401/403
are a distinct credential-rejected state: resolve access in the dashboard before
restarting; do not keep consuming new setup tokens. No automatic credential
replacement occurs. Request/response bodies and raw transport errors are not
logged; structured retry events contain only attempt/delay metadata.

Heartbeats continue during slow signed-update checks and when `auto_update` is
disabled or the binary is a `dev` build. Ctrl+C, process termination, and a staged
update cancel in-flight requests/backoff and join both workers before exit.
Connection changes are printed once rather than on every successful poll.

The dashboard endpoint and its `0100_database_agent_heartbeat` migration must be
deployed in `fivemanage` first. The dashboard marks the agent stale after three
minutes without a heartbeat, independently of any historical backup result.
`idle` means the process is not backing up; it is not proof of a completed backup.
Actual `backing_up`/`uploading` transitions will be supplied by the backup pipeline
when implemented. Runtime state persistence/recovery remains separate work (DEV-501).

### Run as a system service (DEV-491)

Place a built/released binary at its permanent location before installing; the
service records its absolute executable path. Using `go run` to install would
register a temporary build artifact. Windows uses LocalSystem; systemd and
launchd use root. Config/key permissions must allow that account access, and
database client utilities must be on the system/service PATH.

For an already-enrolled agent, register and start without rerunning the wizard:

```powershell
# Elevated Administrator terminal on Windows
.\db-agent.exe install --service-only --config C:\ProgramData\db-agent\config.yaml
.\db-agent.exe stop
.\db-agent.exe start
.\db-agent.exe uninstall
```

```sh
# Linux systemd or macOS launchd; use the permanent binary path
sudo /opt/db-agent/db-agent install --service-only --config /etc/db-agent/config.yaml
sudo /opt/db-agent/db-agent stop
sudo /opt/db-agent/db-agent start
sudo /opt/db-agent/db-agent uninstall
```

`--service-only` requires saved enrollment but permits unfinished DB/key setup
so heartbeat can report a classified setup error. It does not enroll, generate
keys, or certify backup readiness. Normal `install` retains the setup wizard and
prerequisite gate described above.

The service name is `db-agent`. Installation uses automatic startup on Windows,
enables the systemd unit for reboot, and sets launchd `RunAtLoad`. Install/start/
stop/uninstall are safe to repeat; reinstalling does not change an existing
service's executable/config binding. Uninstall and reinstall explicitly to change
those paths. `--config` (or `DB_AGENT_CONFIG`) is resolved to an absolute path at
install time; the installing shell's environment is not required after reboot.
Use absolute encryption-key paths as well. Linux paths containing `%`, `$`,
quotes, backslashes, or line breaks are rejected before writing a unit.

Stopping cancels the root context and waits for diagnostics, heartbeat, update
work, and cleanup. The runtime exposes reverse-order cleanup hooks with a fresh
cancellable context for future backup resources; stop waits at most 15 seconds
before reporting a shutdown error. Windows machine shutdown uses the same path.
Registration stays in setup; the service never persists/reuses an enrollment token
or automatically re-enrolls. A terminal heartbeat failure or staged update also
ends the service host rather than leaving it running with no workers.

Uninstall stops a running service and removes **only its service registration**.
Config, pending enrollment, keys, backups, and logs remain untouched. Controls
work even if local config is missing or damaged. Permission/state failures return
actionable errors; failed systemd units remain startable/removable, while a
Windows service still stopping is not reported as safely stopped.

Runtime output goes to Windows Event Viewer (source `db-agent`), the systemd
journal (`journalctl -u db-agent`), or launchd's `db-agent.out.log`/`db-agent.err.log`
under `/var/log`. Local rotating-file logging is not implemented yet.
Signed update service handoff uses the installed service name on Windows/systemd.
Launchd automatic updates are disabled until its restart helper exists; stop the
service and update its binary manually. Live reboot/stop/update validation on the
target OS is still required; unit tests use fake service managers.

### Control-plane client (DEV-490)

Registration, heartbeat, and typed backup/restore lookups share the same bounded,
authenticated HTTP transport. Registration first negotiates
`GET /v1/agent/register` with `registration_protocol: client-credential-v1`.
An older server is rejected **before** receiving the setup token. The agent
generates a 256-bit candidate credential locally and keeps it in
`<config path>.enrollment-pending`, protected with Unix `0600` or the same Windows
System/Administrators ACL as config. The setup token is never written to disk.

Registration retries reuse that candidate. The server stores only its hash and
binds the consumed setup token to the issued agent. A replay must prove both the
same setup token and candidate, and must not revive revoked/expired access or
rotate the credential. If a response is lost or the process crashes, rerun
`install`: it first tries the pending credential against heartbeat, saves the
confirmed enrollment, and removes the pending file. If registration never
completed, use the **original** setup token to continue. Preserve pending material
after an ambiguous failure; do not delete it or repeatedly request fresh tokens.
No existing config/key is overwritten. New installs require the matching
dashboard registration protocol and migration `0101_agent_registration_metadata`.

The agent reports hostname/OS/architecture/version during registration, and
optional numeric DB driver/version metadata once locally discovered. Connection
details and DB names remain local. The CLI enrolls before connecting the DB, so
its initial registration deliberately omits unknown DB metadata.

Registration and heartbeat may return `config_overrides`. Only backup schedule,
retention, compression, I/O throttling, and `auto_update` are accepted. Unknown
or malformed override fields are rejected without echoing their contents.
Validated fields are saved atomically while preserving credentials, key paths,
API/update URLs, and unknown local fields. Policy can be applied before DB/key
setup finishes. Signed-update checks reread effective policy before each check;
disabling updates prevents subsequent checks, but does not change pinned trust
or interrupt an already authorized replacement. Future backup scheduling uses
the saved policy; local configuration is reread on restart.

Typed client methods define `POST /v1/backups/initiate`,
`POST /v1/backups/{id}/complete`, and `GET /v1/backups/{id}/download`.
Initiation requires an `Idempotency-Key` persisted once per backup run by the
future orchestrator. Completion must be idempotent for the same terminal outcome.
The client validates resource IDs, metadata, checksums, and unexpired HTTPS
transfer URLs; it never sends agent credentials to object storage or performs
the transfer itself. The server-side backup/storage endpoints remain DEV-514;
streaming upload/restore/orchestration remain their separate tasks. See `SPEC.md`
for the exact contracts.

### Logical dump prerequisites

The logical dump executor uses `--single-transaction --quick --routines
--triggers --events --no-tablespaces --skip-lock-tables`. This avoids table locking and the global
`PROCESS` grant needed for tablespace metadata. For each selected database the
backup account needs `SELECT`, `SHOW VIEW`, `TRIGGER`, and `EVENT` privileges.
Routine visibility also needs one of:

- MySQL 8.0.20+: global `SHOW_ROUTINE` (or global `SELECT`).
- MySQL 5.7: `SELECT ON mysql.proc` (or global `SELECT`).
- MariaDB: `SELECT ON mysql.proc`, or global `SHOW CREATE ROUTINE` on 11.3+.

Grants are checked conservatively using the current account's direct grants.
Role-only, table-only, and wildcard-schema grants are not accepted by this first
diagnostic; ask a local administrator for direct grants on the exact selected
schemas. Do not use root as the persisted backup account.

The lock-free snapshot covers InnoDB, not nontransactional tables or concurrent
DDL. The local `backup.dump_timeout` defaults to `2h` and accepts positive values
such as `30m`. When enabled, output throttling defaults to 4 MiB/s. See
[DUMP.md](DUMP.md) for process lifetime and credential-file handling.

Install matching **MySQL client utilities** (`mysqldump`; Linux packages
`mysql-client`/`mysql-community-client`) or **MariaDB client tools**
(`mariadb-dump`; `mariadb-client`). Add their `bin` directory to the system/service
PATH on Windows. The diagnostic selects a client of the same server family and
major version; the client minor version must be at least the server's.
`mariabackup` and `xtrabackup` are detected but are not selected until physical
dump execution and filesystem checks exist.

Use `db-agent configure --key` to set up and acknowledge the local recovery key.
Diagnostics verify its encoding, private Unix permissions/Windows ACLs, saved
fingerprint, and acknowledgement. Never upload this file or
paste it into support tickets. API reachability makes an unauthenticated GET to
the configured base URL, sends no agent/DB secrets, and does not follow redirects.
HTTP 4xx proves the host is reachable, not that agent authentication works.

## Development checks

```sh
go test ./...
go vet ./...
```

Isolated local integration fixtures cover MySQL 8.4 and MariaDB 11.4, including
bad passwords, missing/hidden databases, and insufficient privileges. These
containers use development-only credentials, publish loopback-only ports, and
do not reuse the main development database or its volume.

```sh
docker compose -p db-agent-diagnostics -f docker-compose.diagnostics.yml up -d --wait
DB_AGENT_INTEGRATION=1 go test -tags integration ./internal/database -v
docker compose -p db-agent-diagnostics -f docker-compose.diagnostics.yml down --volumes
```

Optional `DB_AGENT_MYSQL_PORT` and `DB_AGENT_MARIADB_PORT` override fixture ports.
See `SPEC.md` for the MVP plan and `UPDATE.md` for signed updates.

To test actual local-account creation and logical dumps, run a static test binary
inside each isolated fixture (the `@localhost`/loopback identities intentionally
cannot authenticate through the host's Docker bridge connection):

```sh
CGO_ENABLED=0 go test -c -tags integration -o /tmp/opencode/db-agent-database.test ./internal/database
docker cp /tmp/opencode/db-agent-database.test db-agent-diagnostics-mysql-1:/tmp/db-agent-database.test
docker exec -e DB_AGENT_PROVISION_INTEGRATION=1 db-agent-diagnostics-mysql-1 /tmp/db-agent-database.test -test.run '^TestProvisionedLocalAccountCanDump$' -test.v
docker cp /tmp/opencode/db-agent-database.test db-agent-diagnostics-mariadb-1:/tmp/db-agent-database.test
docker exec -e DB_AGENT_PROVISION_INTEGRATION=1 db-agent-diagnostics-mariadb-1 /tmp/db-agent-database.test -test.run '^TestProvisionedLocalAccountCanDump$' -test.v
```

Run this before stopping the fixtures. It checks routines/views/triggers/events
in the dump, denial of access to a similarly named schema, and safe reruns.
