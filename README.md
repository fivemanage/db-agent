# db-agent

Customer-hosted MySQL/MariaDB backup agent for FiveManage. **Work in progress:**
registration, local database setup, recovery-key generation, diagnostics,
foreground/service heartbeats, signed updates, scheduled/manual encrypted backups/uploads,
and local SQL recovery exist. Dashboard-triggered restore/import is implemented,
with release qualification still pending under DEV-577. A passing diagnostic or
running service does not mean a backup exists.

The streaming backup codec is implemented in `internal/artifact`: zstd (default)
or gzip, locally encrypted with chunked AES-256-GCM, with encrypted size/SHA-256
metadata and restore decoding. `backup --now` runs this pipeline. See
[ARTIFACT.md](ARTIFACT.md) for the versioned format, security constraints, and
backup/restore integration.

The scoped logical dump executor is implemented in `internal/dump`, with private
client credentials, timeouts, process-tree cancellation, and output throttling.
It is wired into `backup --now`. See [DUMP.md](DUMP.md) for its contract,
consistency limits, and real MySQL/MariaDB integration tests.

## Build

```sh
make build      # current platform: bin/db-agent (bin/db-agent.exe on Windows)
make build-all  # Windows amd64, Linux amd64/arm64, macOS arm64 binaries in bin/
make check      # Go tests and vet
```

Builds default to version `dev`; override it with `make build VERSION=0.1.0`.
These are unsigned development builds. See [RELEASE.md](RELEASE.md) for tagged
release CI, secret-free builds, signing, provenance, and verification.
`go run . <command>` compiles the current source
automatically, so it does not require a separate build.

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

For the local dashboard, set `api_url: "http://localhost:3000"`, without `/api`
or `/api/v1`. The agent appends `/api/v1/agent/...` and `/api/v1/backups/...` itself.
The dashboard retains `/v1/...` as direct compatibility aliases for older agents;
new agents use `/api/v1/...` without redirects.

`install` saves the issued agent credential **before** database setup. Rerunning
it resumes without another one-time token and preserves existing database
settings and key material. Use `db-agent configure --config ./config.yaml` to
retry failed database setup or explicitly change an existing connection.

Setup displays five numbered stages: enrollment, database connection, recovery
key, prerequisite checks, and service startup. Install, configure, and help share
a compact ASCII "DATABASE AGENT" header. Connection details are grouped for
review without showing passwords. Status labels use color on supported terminals
and stay plain text when redirected; set `NO_COLOR=1` or `TERM=dumb` to disable
color. The wizard does not clear or redraw the terminal, and a completed setup
does not claim that the first backup has finished.

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
to create `db_agent_backup` with a cryptographically generated password after
showing the SQL and requiring explicit approval. It uses `USER()` to discover
the exact client host seen by the database. For Docker-published ports, this is
typically a bridge address rather than localhost; only that exact address gets
an account. Native local connections retain both `@localhost` and the observed
loopback IP (or selected loopback IP when the server reports localhost), with
the same password/grants, to handle name-resolution differences. Destination
connections remain loopback-only, and `localhost` input is pinned to IPv4 TCP.
No wildcard-host identity is created. Setup rechecks the observed host before
creating any accounts and requires fresh approval if it changes. If discovery
fails or reports an unsafe host, use an existing backup account instead. It does
**not** alter an existing account or reset its password. Schema grants escape
wildcard characters to restrict access to the confirmed database. The new account is tested before
saving; grant, verification, or config-save failure removes only the identities
confirmed created by that attempt. If cleanup fails, ask a local administrator
to inspect and remove those new identities before retrying. An ambiguous CREATE
failure never triggers automatic removal of a possibly pre-existing account.

If creation is declined or unavailable, supply an existing backup account with
the displayed schema/routine grants. Root and accounts with detected admin or
grant-authority privileges cannot be saved by the wizard. Admin credentials are
ephemeral; only a tested, confirmed backup account is persisted. The wizard
offers a retry after failed steps; enrollment does not need to be repeated.

During the database step, `install` asks **Enable dashboard restores? [y/N]**.
Skipping keeps setup backup-only. Opting in shows the separate `db_agent_restore`
account's SQL and sensitive global account-management permission, and requires
typing `AUTHORIZE` before creation. Setup reuses the temporary admin connection
when available; otherwise it asks once for a local administrator's username and
hidden password, pinned to the saved database server. Admin credentials are never
saved. Only the exact server-observed client host is granted access, with data
permissions confined to the escaped `fm_restore_` namespace. The account is
verified before separate restore credentials are saved locally; the backup account
stays read-only. No restore or production cutover runs during setup.

Rerun `install` to enable restores on an existing installation. Already-enabled
access is rechecked and preserved without recreating accounts. A previously
disabled saved account can be re-enabled after verification and `AUTHORIZE`.
Creation never changes an existing SQL account; use `configure --restore-access`
to authorize one instead. Grant, verification, or save failure removes only the
restore account confirmed created by that attempt and preserves backup setup/key
material. Ambiguous creation or failed cleanup requires local inspection.

Changes replace the config privately while retaining other settings. Database
credentials stay on the local machine and are never included in the API probe.
Alternatively edit `database` in the private config using the example in `SPEC.md`.
No database is implicitly selected when `databases` is empty.

Release builds can set `cmd.defaultAPIURL` via Go linker flags to avoid asking
users for an API URL. Until a production endpoint is configured, development
builds require `--api-url` for initial enrollment; resumed setup uses the saved
URL. `install` sets up the recovery key after database setup, runs diagnostics, and installs
and starts the service only if all prerequisites pass. Failed checks leave any
existing service unchanged. The service attempts an initial backup in the background
when local prerequisites pass and no earlier run is recorded. Installation does not
wait for an upload; use `db-agent status` to check the first outcome.

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
heartbeat to `/api/v1/agent/heartbeat`, then polls every 60 seconds. It uses only the
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
update cancel in-flight requests/backoff and join heartbeat, backup/recovery, and
update workers before exit.
Connection changes are printed once rather than on every successful poll.

The dashboard endpoint and its `0100_database_agent_heartbeat` migration must be
deployed in `fivemanage` first. The dashboard marks the agent stale after three
minutes without a heartbeat, independently of any historical backup result.
`idle` means the process is not backing up; it is not proof of a completed backup.
Heartbeats read shared private backup state, including manual runs in another
process, and report `backing_up`, `uploading`, and terminal idle/error states.
`db-agent status` shows recorded backup state and the next scheduled run; live
service health remains separate work (DEV-501).

### Run a backup now (DEV-494)

```sh
./db-agent backup --now --config ./config.yaml
```

Use the installed agent's config and the intended service/Administrator account.
The command validates local prerequisites and executes the selected logical dump,
compression, encryption, throttled upload, and completion report. It also runs
heartbeats independently and reports terminal state before exit. Success means
the upload and its completion were acknowledged, not just that the dump exited.
The control plane must provide the endpoints/idempotency contract in
[TRANSFER.md](TRANSFER.md) (DEV-514); this command does not deploy them.

A whole-run OS lock excludes manual, scheduled, startup runs, and transfer recovery.
A second trigger fails with an already-running
message without launching a dump. Queued transfers/completions block new backups
until the running agent resolves them after an accepted heartbeat.

Only ciphertext is spooled, with a 1 GiB cap and 256 MiB disk reserve. Headroom
is checked before spooling and before every ciphertext write. Partial artifacts
are removed on failure, and the dump process and workers are joined before
network reporting. Private `backup-transfer/run.json` beside config retains the
latest run's identity, timing, outcome, encrypted size/checksum, pending status,
and classified last error; no SQL, credentials, keys, or signed URLs are saved.
See [BACKUP.md](BACKUP.md) for lifecycle and recovery details.

### Scheduled backups (DEV-498)

The foreground agent and service use `backup.schedule`, defaulting to `0 4 * * *`
(4am in the machine's local timezone). Use five cron fields: minute, hour, day of
month, month, and day of week. Seconds, `@daily`/`@every`, and `TZ=`/`CRON_TZ=`
prefixes are rejected. Invalid or impossible schedules fail configuration validation
without echoing their contents.

Local schedule edits and timezone changes take effect on service restart. Validated
heartbeat schedule overrides are saved and picked up within one second; they do
not interrupt a running backup. Each new run reads saved backup policy. Backups
continue during transient heartbeat outages, but rejected credentials stop and join
all work. A ready agent with no recorded run attempts one initial backup at startup;
subsequent restarts do not force another dump or replay missed cron occurrences.

Overlapping ticks are skipped, not queued. In-flight clock jumps forward consume
at most one due run; backward corrections do not repeat an already-consumed instant.
Calendar/DST behavior follows robfig/cron: nonexistent local times are skipped,
and a repeated hour can have two distinct occurrences. The default 4am avoids
the usual DST transition hour.

```sh
./db-agent status --config ./config.yaml
```

The private `backup-transfer/schedule.json` records the expression, observation time,
and next run with a timezone offset, independently of run history. Graceful stop
clears the next run. After a crash the timestamp may be stale: this command shows
**recorded state, not live service health**. Remote "Back up now" polling is deferred:
the current heartbeat response has no stable request/acknowledgement contract.

### Recover SQL from a backup (DEV-499)

```sh
db-agent restore --backup-id <id>
```

**The existing local key is used automatically.** No key paste, terminal prompt,
or upload is needed. Run as the installed service/Administrator account with the
same private config (use `--config` if it is not at the default location).
Restore does not require the original database, dump tools, or a working backup
schedule. It writes `restore.sql` by default; `--output <new-path>` chooses another
file. Existing files are never overwritten.

For an already-downloaded backup, the same local key is used automatically:

```sh
db-agent restore --file ./backup.dbak --output ./recovered.sql
```

If the original server was lost, use an encrypted file and the original saved
key **file** on the replacement machine; no config, enrollment, or API access is
required:

```sh
db-agent restore --file ./backup.dbak --key-file ./recovery.key
```

The saved key file must be private (`0600` on Unix; protected
System/Administrators ACL on Windows). Never put key contents in command-line
arguments. Without either the original local key or its saved copy, restoration
is impossible; FiveManage has no copy.

**This command recovers SQL; it does not import it or change a database.** The
SQL contains the original database names, including `CREATE DATABASE` and `USE`.
Selecting another default database in the SQL client does **not** redirect it.
Use an isolated restore server for manual import, following the printed guidance.
Do not import into production or attempt schema remapping with text replacement.

Defaults bound the complete operation to two hours, encrypted input to 1 GiB,
and SQL output to 16 GiB, retaining 256 MiB of free disk space. `--timeout` and
`--max-output-bytes` can explicitly adjust duration/output limits. SQL is staged
privately, fully authenticated/decompressed, and only then published. Handled
failures/cancellation remove staged output. A hard crash can leave a protected
`.db-agent-restore-*` file; an administrator should remove it after confirming no
restore is running. Verified SQL is sensitive: protect and remove it when done.

See [RESTORE.md](RESTORE.md) for the download/integrity contract. Dashboard
Restore is implemented separately: it uses the installed key automatically,
imports supported SQL into a new isolated database, and reports progress/outcome.
It never switches the application or overwrites production. Routines/triggers/
events and unsupported SQL stop the entire job before writes. This flow still
needs release qualification, not just successful SQL recovery tests; see
[RESTORE-JOBS.md](RESTORE-JOBS.md) and
[RESTORE-QUALIFICATION.md](RESTORE-QUALIFICATION.md).

Supported tables preserve enforced `CHECK (JSON_VALID(column))` constraints and
ordinary FULLTEXT indexes over local text columns. Other CHECK expressions and
FULLTEXT parser plugins remain unsupported. MariaDB CHECK enforcement is enabled
for the import session; MySQL dumps containing these checks require MySQL 8.0.16+
so constraints cannot be silently ignored by an older server.

MariaDB dump-client AUTOCOMMIT save/disable, COMMIT, and restore blocks are
recognized as bookkeeping and never replayed. Import keeps autocommit enabled;
incomplete/nested blocks and other transaction-control SQL remain unsupported.

Dashboard restores accept up to **8 GiB of uncompressed SQL** and **8 GiB of
encrypted input**. Preflight validates the entire dump before database writes,
then imports using hashed references to the private SQL file rather than holding
all row SQL in RAM. Statements (4 MiB), raw frames including comments (8 MiB),
statement count (1,000,000), and table/view objects (4096) remain bounded. The
two-hour job deadline and 256 MiB free-disk reserve still apply. This does not
raise the separate current 1 GiB encrypted backup-upload staging limit.

Installation offers to create and authorize the separate restore account as
described above. For an existing administrator-prepared account, local setup is available with
`db-agent configure --restore-access --config ./config.yaml`. It verifies an
**existing dedicated provisioning account** and saves separate credentials only
after explicit confirmation; it does not create users, change grants, run an
import, or contact the dashboard. The running agent advertises readiness separately.
Enter `?` at the username prompt for the administrator's account-creation SQL
and exact-host instructions. Do not enter `root` or your game-server/backup login.
This authority includes global
SQL account-management permission and must never execute backup SQL directly.
See [RESTORE-ISOLATION.md](RESTORE-ISOLATION.md#explicit-local-restore-access)
for the permission template and restrictions. Never
paste database passwords or recovery keys into chat or the dashboard.

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

Stopping cancels the root context and waits for diagnostics, heartbeat, scheduling,
backup/recovery, update work, and cleanup. The runtime exposes reverse-order cleanup
hooks with a fresh cancellable context for resource cleanup; stop waits at most 15 seconds
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
`GET /api/v1/agent/register` with `registration_protocol: client-credential-v1`.
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
or interrupt an already authorized replacement. Scheduling uses the saved policy
and applies heartbeat schedule changes on its next clock sample; local schedule edits are
reread on restart.

Typed client methods define `POST /api/v1/backups/initiate`,
`POST /api/v1/backups/{id}/complete`, and `GET /api/v1/backups/{id}/download`.
Initiation requires an `Idempotency-Key` persisted once per backup run by the
orchestrator. Completion must be idempotent for the same terminal outcome.
The client validates resource IDs, metadata, checksums, and unexpired HTTPS
transfer URLs; it never sends agent credentials to object storage or performs
the transfer itself. Download lookup requires trusted encrypted size and SHA-256
metadata. Transfers are implemented separately in `internal/transfer` and
`internal/restore`; the matching DEV-514 endpoints/storage must be deployed in
the control plane. See `SPEC.md` for the exact contracts.

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
`<api_url>/api/v1/agent/register`, not the dashboard root, sends no agent/DB secrets,
and does not follow redirects.
HTTP 404/405 fails because the agent endpoint is missing. Other HTTP 4xx only
proves the host is reachable, not that agent authentication works.

## Development checks

```sh
go test ./...
go vet ./...
```

Isolated local integration fixtures cover MySQL 8.4 and MariaDB 11.4, including
bad passwords, missing/hidden databases, insufficient privileges, and automatic
account creation through Docker's published ports using the observed bridge host.
These containers use development-only credentials, publish loopback-only ports, and
do not reuse the main development database or its volume.

```sh
docker compose -p db-agent-diagnostics -f docker-compose.diagnostics.yml up -d --wait
DB_AGENT_INTEGRATION=1 go test -tags integration ./internal/database -v
docker compose -p db-agent-diagnostics -f docker-compose.diagnostics.yml down --volumes
```

Optional `DB_AGENT_MYSQL_PORT` and `DB_AGENT_MARIADB_PORT` override fixture ports.
See `SPEC.md` for the MVP plan and `UPDATE.md` for signed updates.

To also test native local-account creation and logical dumps, run a static test
binary inside each isolated fixture. This exercises `@localhost`/loopback
identities separately from the host-side bridge-account test above:

```sh
CGO_ENABLED=0 go test -c -tags integration -o /tmp/opencode/db-agent-database.test ./internal/database
docker cp /tmp/opencode/db-agent-database.test db-agent-diagnostics-mysql-1:/tmp/db-agent-database.test
docker exec -e DB_AGENT_PROVISION_INTEGRATION=1 db-agent-diagnostics-mysql-1 /tmp/db-agent-database.test -test.run '^TestProvisionedLocalAccountCanDump$' -test.v
docker cp /tmp/opencode/db-agent-database.test db-agent-diagnostics-mariadb-1:/tmp/db-agent-database.test
docker exec -e DB_AGENT_PROVISION_INTEGRATION=1 db-agent-diagnostics-mariadb-1 /tmp/db-agent-database.test -test.run '^TestProvisionedLocalAccountCanDump$' -test.v
```

Run this before stopping the fixtures. It checks routines/views/triggers/events
in the dump, denial of access to a similarly named schema, and safe reruns.
