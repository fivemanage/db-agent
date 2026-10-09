# db-agent — MVP Specification

## What this is

A small, cross-platform Go binary that runs on a customer's game server, takes
periodic backups of their local MySQL/MariaDB database, encrypts them, and
uploads them to object storage. It reports status back to a control-plane API so
the customer can see backup health in a web dashboard.

**Primary audience:** FiveM game server owners. Mostly Windows Server / Windows
VPS, some Linux. Technical enough to run a game server, generally *not*
sysadmins. Install friction is the single biggest adoption risk — a setup that
takes more than a couple of minutes will lose a large share of users.

**Core promise to the user:** "Your database is backed up automatically, you can
see that it's working, and you can get your data back if something breaks."

---

## Non-goals for the MVP

Explicitly out of scope. Do not build these:

- Query performance monitoring, slow-query logs, connection-count metrics
  (this is a separate observability product, not backup)
- Point-in-time recovery / binlog streaming (v2 candidate)
- Table-level analytics (row counts, per-table sizing)
- Managed/hosted database provisioning (the object model should not *prevent*
  this later, but the agent has nothing to do with it)
- Live-streaming dashboards / websockets
- Restore automation *from the agent* (see "Restore" below — MVP restore is
  download + documented manual steps)

---

## Architecture overview

```
┌────────────────────────── customer's server ──────────────────────────┐
│                                                                        │
│   MariaDB/MySQL  ←── localhost only ──  db-agent (Go binary, service)  │
│                                              │                         │
└──────────────────────────────────────────────┼─────────────────────────┘
                                               │ HTTPS (outbound only)
                                               ▼
                              ┌────────────────────────────┐
                              │  Control plane API         │
                              │  - agent registration      │
                              │  - heartbeat               │
                              │  - backup event reporting  │
                              │  - issues presigned upload │
                              └──────────┬─────────────────┘
                                         │ presigned PUT
                                         ▼
                              ┌────────────────────────────┐
                              │  Object storage (S3/R2)    │
                              └────────────────────────────┘
```

**Critical design constraints:**

1. **Outbound-only.** The agent initiates every connection. Nothing ever
   connects *into* the customer's machine. No inbound ports, works behind NAT,
   works with dynamic IPs.
2. **DB credentials never leave the machine.** The agent connects to the
   database over `localhost`. Those credentials are stored locally and are never
   transmitted to the control plane.
3. **Encrypt before upload.** Backups are encrypted client-side. The control
   plane and object storage only ever hold ciphertext.
4. **The agent authenticates with a database-scoped credential**, not an
   account-wide API key. The installer exchanges a short-lived enrollment token
   for this credential and discards the enrollment token. One agent is bound to
   one `Database` object.

---

## Object model (control plane side, for context)

The agent binds to exactly one `Database` object. The customer creates this in
the dashboard *first*, and receives a short-lived enrollment token.

```
Database
  id            string        // stable identifier
  name          string        // user-provided, e.g. "ESX Production"
  type          enum          // "external" (MVP) | "managed" (future, unused)
  status        enum          // never_connected | healthy | stale | failing
  last_seen_at  timestamp     // from heartbeat
  ...
```

Enrollment tokens and agent credentials are separate records scoped to the
Database. Store only their hashes in the control plane.

The `type` field exists in the MVP schema but is always `"external"`. It is
there so a future managed-hosting product can reuse the same object, dashboard,
and billing surface without a migration.

---

## Configuration

Stored locally in a config file. Platform-appropriate location:

- Windows: `C:\ProgramData\db-agent\config.yaml`
- Linux/macOS: `/etc/db-agent/config.yaml`

```yaml
# Written by the installer, editable by hand.
agent_credential: "dac_xxxxxxxxx"   # issued at enrollment; never shown in dashboard
api_url: "https://api.example.com"   # control plane base URL
auto_update: true
# Optional public signed release feed, independent of the control plane API.
# update_url: "https://releases.example.com/db-agent/{os}-{arch}.json"

database:
  driver: "mysql"                    # mysql | mariadb
  host: "127.0.0.1"
  port: 3306
  user: "db_agent_backup"
  password: "<generated>"
  # Explicitly confirmed scope; an empty list is incomplete setup.
  # System databases are excluded from this logical backup path.
  databases: ["es_extended"]

backup:
  schedule: "0 4 * * *"              # cron expression, default 4am local
  retention_days: 7                  # informational; enforced control-plane side
  compression: "zstd"                # zstd | gzip
  throttle_io: true                  # rate-limit to avoid impacting the server
  dump_timeout: "2h"                # positive local-only execution duration

encryption:
  # Key is generated at install time and shown ONCE to the user.
  # Agent stores it locally. Control plane never sees it.
  key_file: "C:\\ProgramData\\db-agent\\encryption.key"
  # Setup also writes key_id (SHA-256 fingerprint) and key_acknowledged.
  # Do not invent these fields or bypass recovery-copy confirmation.
```

**Config file permissions must be restrictive** (0600 equivalent / Administrators
only on Windows) — it contains DB credentials and the path to the encryption key.

---

## Core behaviours

### 1. Registration (once, at install)

```
POST /v1/agent/register
Authorization: Bearer <enrollment token>

{
  "agent_version": "0.1.0",
  "hostname": "GAMESRV-01",
  "os": "windows",
  "arch": "amd64"
}
```

The safe retry protocol first discovers `registration_protocol:
"client-credential-v1"` using unauthenticated `GET /v1/agent/register`. Its POST
also contains `agent_credential`, a locally generated 256-bit `dac_` candidate.
The agent protects this candidate in `<config path>.enrollment-pending` before
the exchange and never persists the enrollment token. The server stores only
the candidate hash, atomically binds the consumed token to the issued agent,
and accepts a replay only with the same consumed token and matching active
candidate. Different candidates, expired/revoked enrollment tokens, and revoked
agents/credentials cannot replay. Legacy requests without a candidate remain
one-shot; a new client refuses legacy capability responses before sending a token.

After a lost response/process restart, setup probes the pending credential with
heartbeat to confirm an already-issued enrollment without another setup token.
An unissued candidate requires the original token to continue with the same
candidate. Recovery never overwrites an existing config/key or rotates secrets.
Successful recovery saves config and removes pending state.

Database driver and version are reported later, after local DB setup. Response
confirms the binding, returns the agent credential, and may return
server-side config overrides (e.g. schedule). The installer persists only the
agent credential. If the enrollment token is invalid, the agent should fail loudly with a
clear message — this is the #1 install-time error and the message needs to be
human-readable, not a stack trace.

Optional `db_driver` (`mysql`/`mariadb`) and numeric `db_version` are accepted on
registration and heartbeat when known. Hostname is persisted with agent metadata.
Neither message contains DB connection settings, database names, or key material.

Registration/heartbeat responses may contain `config_overrides`:

```json
{
  "config_overrides": {
    "backup": { "schedule": "0 5 * * *", "retention_days": 7,
                "compression": "zstd", "throttle_io": true },
    "auto_update": false
  }
}
```

Only these fields are permitted. Overrides are validated independently of
unfinished DB/key setup and saved through a private atomic config replacement,
preserving all local-only and unknown fields. API/update URLs, credentials,
encryption keys/paths, and update trust cannot be overridden. Unknown override
fields fail closed; unrelated response-envelope fields are ignored. Schedule
expressions retain the local-time contract; no remote timezone override is defined.
Signed-update checks read saved policy before each check; an already authorized
replacement is not interrupted by a later policy change.

### 2. Heartbeat (every 60s)

```
POST /v1/agent/heartbeat
Authorization: Bearer <agent credential>

{
  "agent_version": "0.1.0",
  "status": "idle" | "backing_up" | "uploading" | "error",
  "last_error": "optional string"
}
```

This is deliberately separate from backup reporting. "Is the agent alive" and
"did the last backup work" are different questions and the dashboard must be
able to answer both independently. A dead agent with a successful backup from
three days ago should read as *unhealthy*, not healthy.

The foreground and service agent currently send an immediate heartbeat after startup
diagnostics and polls every 60 seconds on Windows and Unix. Local prerequisite
failures produce a classified `error` message; raw DB errors, config, encryption
keys/paths, and diagnostic output never enter the heartbeat payload. The current
activity comes from shared private backup state so manual and automatic runs supply
`backing_up`/`uploading` transitions without changing transport code.

Each poll is bounded to 45 seconds with 15-second HTTP timeouts, up to three
attempts, and jittered exponential backoff for transport failures, HTTP 429, and
HTTP 5xx. `Retry-After` cooldowns survive the individual poll budget. Permanent
HTTP failures, redirects, and malformed responses stop the process; HTTP 401/403
produce a distinct rejected-credential result and require access recovery rather
than automatic re-enrollment. Heartbeats run independently of signed-update
checks. Cancellation and update handoff join the workers before process exit.
The control plane derives staleness at 180 seconds; a heartbeat does not overwrite
backup outcomes. Backup activity/latest outcome are now read from shared private
run state, including manual runs in another process. `db-agent status` exposes
recorded backup outcomes and next scheduled time; live service health remains DEV-501.

### 3. Backup run

Triggered by schedule, or on demand (see "Manual trigger" below).

The logical dump execution stage is implemented in `internal/dump` (DEV-496).
It streams only the explicitly selected local application databases, using the
compatible discovered client, a private temporary credentials file, and no SQL
dump files. It isolates/kills child process trees, joins workers, lowers client
priority, optionally limits stdout to 4 MiB/s, and uses local `backup.dump_timeout`
(default two hours). Private options, bounded stderr, classified errors, and
cleanup are specified in `DUMP.md`. The lock-free snapshot covers InnoDB, not
nontransactional tables or concurrent DDL. Physical execution/all-visible scope
remain deferred. `backup --now` connects this executor to the encrypted transfer
pipeline through the whole-run lock and durable state in `internal/backup` (DEV-494).

Steps:

1. Report `status: backing_up` via heartbeat.
2. Dump the database:
   - **Initial path:** `mysqldump` / `mariadb-dump` with `--single-transaction
       --quick --routines --triggers --events --no-tablespaces`.
      Execution explicitly disables table locking and emits binary columns in
      hex. MySQL disables GTID purging and MySQL 8+ column statistics; MariaDB
      does not receive MySQL-only flags. No global RELOAD/PROCESS grants are
      required by this logical path.
   - **Future preferred path:** `mariabackup` / `xtrabackup` (physical backup —
      much lower load, supports incremental later), after version compatibility,
      physical backup privileges, and local filesystem access are validated.
      Discovery alone must not declare a physical backup ready.
   - Detect which is available at startup and log which one will be used.
3. Stream the dump through compression (zstd default) and then through
   encryption — **do not** write plaintext or unencrypted compressed temporary
   artifacts. Stream: `dump → compress → encrypt → upload`.
4. Request a presigned upload URL from the control plane:

```
POST /v1/backups/initiate
Authorization: Bearer <agent credential>
Idempotency-Key: <stable per-run identity>

{ "estimated_size_bytes": 348000000, "started_at": "..." }

→ { "backup_id": "...", "upload_url": "https://...", "expires_at": "..." }
```

The agent client defines this contract now; server-side transfer behavior is
implemented in DEV-514. The orchestrator must persist one CSPRNG-generated
request identity for each run, reuse it on retry/restart, and never generate a
new identity for the same initiation. The server must return the same bound
backup for that identity, rather than creating duplicate records. Unknown size
estimates are omitted. Responses may use HTTP 200 or 201 and must include an
unexpired HTTPS upload URL. No object-storage request receives the agent credential.

5. `PUT` the encrypted stream to `upload_url`.
6. Report completion:

```
POST /v1/backups/{backup_id}/complete
Authorization: Bearer <agent credential>

{
  "status": "success" | "failed",
  "size_bytes": 341234567,
  "duration_seconds": 142,
  "checksum": "sha256:...",
  "error": "optional, if failed"
}
```

7. Report `status: idle`.

Completion is idempotent for identical terminal metadata; conflicting outcomes
must be rejected rather than overwriting history. Successful completion requires
a nonnegative encrypted size/duration and `sha256:<64 lowercase hex digits>`.
Failed completion uses classified safe errors, never raw dump/DB/network output.
Resource IDs are bounded opaque alphanumeric/underscore/hyphen strings; they
cannot inject paths. A successful completion response is HTTP 200 with `{}`.

Ciphertext transfer and durable completion replay are implemented in
`internal/transfer` (DEV-495); see `TRANSFER.md` for the retry/renewal contract,
single-slot private spool, disk reserve, cancellation, and recovery behavior.
Encryption stages at most 1 GiB of ciphertext before streaming PUT, making
identical retries possible without another dump. Three bounded transfer sessions
and a 24-hour artifact age limit prevent unbounded local retention. Terminal
reports are saved before sending and retried by an independent worker after a
successful heartbeat. Dump wiring, whole-run activity/locking, and manual runs
are implemented in `internal/backup`; see `BACKUP.md`. Scheduling is implemented
there too (DEV-498), with default 4am local, validated five-field cron, saved/live
heartbeat schedule overrides, and an initial startup attempt for ready agents
without a prior run. All triggers enter the same whole-run lock. Busy ticks are
skipped rather than queued; restart does not catch up missed ticks. The next run
is privately recorded independently of run history and cleared on graceful stop;
crash-stale records must not imply service health. Deterministic tests cover local
offsets, DST, forward/backward clock corrections, and config/timezone restarts.
The control-plane transfer endpoints remain DEV-514 work.

### 4. Manual trigger

The agent should support an on-demand backup:

- CLI: `db-agent backup --now`
- A control-plane-initiated trigger picked up on the next heartbeat remains
  deferred: the current endpoint returns no stable request/acknowledgement contract.

---

## Load safety (important — this is a live game server)

The agent runs alongside a production game server. Causing lag is worse than
failing to back up.

- **Default schedule to a low-traffic hour** (4am local) and make it
  configurable.
- **Use `--single-transaction`** so `mysqldump` does not lock tables (InnoDB).
  Never use `--lock-all-tables` by default.
- **I/O throttling:** rate-limit read and upload throughput. `mariabackup`
  supports `--throttle`. For `mysqldump`, throttle at the pipe level.
- **Nice / low process priority** where the OS supports it
  (`syscall` on Linux, `SetPriorityClass` BELOW_NORMAL on Windows).
- **Abort cleanly** if the process is taking pathologically long, and report the
  failure rather than running indefinitely.
- **Never run two backups concurrently.** Use a local lock file.

---

## Encryption

- Symmetric, **AES-256-GCM** (or `age` if you prefer a well-audited envelope
  format — either is acceptable, pick one and document it).
- Key generated locally at install time, 256-bit, from a CSPRNG.
- Key written to `encryption.key` with restrictive permissions.
- **The key is displayed to the user exactly once at install** with a clear
  warning: *"Save this key somewhere safe. Without it, your backups cannot be
  restored. We do not have a copy."*
- Control plane must never receive or store the key. This is both a genuine
  security property and a strong marketing point — say it plainly in the docs.

Key lifecycle is implemented independently of the backup artifact format.
The local/recovery encoding is `dbak1_` plus the canonical 43-character unpadded
base64url encoding of 32 CSPRNG bytes; the private file adds a trailing newline.
Config contains only the absolute key path, SHA-256 fingerprint, and acknowledgement.
Key setup defaults to `encryption.key` beside config, resolving existing relative
key paths against that directory. Unix uses `0600`; Windows uses a protected
System/Administrators DACL. Config/key readers reject unsafe permissions, links,
nonregular files, and oversized data. New files are privately staged and synced
before no-overwrite publication; config changes preserve unrelated settings via
private atomic replacement. Existing key material is never rotated on retry.

`install` and `configure --key` use an interactive, unredirected terminal. The
new key is shown once directly to the terminal, then the user must paste the saved
copy through hidden input and type `SAVED`. Terminal recording/transcripts must
be disabled. An existing file consumes the display opportunity, including a crash
before/after display or a failed terminal write; retries require confirmation from
the original saved copy without redisplay. If the display was lost, a local admin
must copy the protected key file to offline storage and confirm that copy. A
configured missing, malformed, unsafe, or fingerprint-mismatched key blocks setup
without generating a replacement. No key contents, admin credentials, or imported
connection strings enter config, logs, errors, or control-plane messages. Only the
tested scoped DB account is persisted in local config.

The version-1 streaming artifact codec is implemented in `internal/artifact`;
its exact header, HKDF-SHA-256/AES-256-GCM record layout, authenticated terminal
record, zstd/gzip settings, limits, and independent test vector are specified in
`ARTIFACT.md`. It counts/hashes the complete encrypted artifact while streaming
and creates no files. Backup scheduling/dump orchestration are implemented; the
restore CLI remains separate work. Restore must discard partially decoded output
on failure and publish only after full authentication, decompression, and EOF checks succeed.

Consider offering an optional "escrow the key with us" mode later for users who
would rather trade zero-knowledge for not-losing-their-key. **Not in the MVP.**

---

## Restore (MVP scope)

Restore does **not** need to be fully automated in v1, but it must be
*documented and possible*, or the whole feature is theatre.

MVP restore path:

```
db-agent restore --backup-id <id> --output ./restore.sql
```

- Downloads the encrypted backup (via a presigned download URL from the control
  plane), decrypts it locally with the key file, decompresses it.

The typed lookup contract is authenticated `GET /v1/backups/{id}/download`,
returning HTTP 200 with `{ "download_url": "https://...", "expires_at": "..." }`.
The URL must be unexpired HTTPS without userinfo/fragment. Lookup and transfer are
separate: the client does not fetch or forward agent credentials to the returned
URL. Object-storage ownership, download issuance, and expired-URL renewal are
server-side DEV-514 work; decryption/output are DEV-499 work.
- Writes the plain dump to disk.
- Prints the exact command the user should run to import it, e.g.
  `mysql -u root -p dbname < restore.sql`.

Do **not** have the agent write directly into the live database in v1. Handing a
tool the ability to overwrite a production database is a footgun; make the final
destructive step a deliberate human action.

---

## CLI surface

Keep it small.

```
db-agent install          # register as a system service, run setup wizard
db-agent uninstall        # remove service, leave config + key in place
db-agent start | stop     # service control
db-agent status           # local status: last backup, next scheduled, agent health
db-agent backup --now     # trigger a backup immediately
db-agent restore --backup-id <id> --output <path>
db-agent test             # verify DB connectivity + API reachability, print result
db-agent configure        # connect an already-enrolled agent and confirm DB scope
db-agent version
```

`db-agent test` is worth building early. It turns "it's not working" support
tickets into self-diagnosis.

The current cross-platform setup wizard enrolls before DB setup and can resume
using the issued agent credential. It accepts a hidden game-server connection
string or manual local TCP settings, confirms the selected application database,
and tests the connection before changing SQL or saving database settings. With
direct account-creation permission it offers the scoped SQL below for explicit
approval, then verifies the generated account before saving. Otherwise it accepts
a confirmed existing backup account, but refuses root/detected admin accounts.
Existing accounts are never altered. Setup generates/confirms the key and
installs/starts the service only after diagnostics pass. The service attempts a first
backup in the background when ready and no previous run is recorded; installation
does not wait for the outcome or claim an acknowledged upload.

The current diagnostic checks config, DB reachability/authentication, numeric
server version, explicit scope, direct logical dump grants, compatible client
tooling, local key format/private permissions/fingerprint/acknowledgement, and
unauthenticated API host reachability independently. It does not execute a backup or verify
agent authentication. See `README.md` for requirements and local fixtures.

---

## Service installation

Must run as a background service so it survives reboots and does not require a
logged-in session (essential for VPS users).

- **Windows:** register as a Windows Service. Recommended:
  `github.com/kardianos/service` — handles Windows Service, systemd, and launchd
  from one codebase, which is exactly the cross-platform need here.
- **Linux:** systemd unit.
- **macOS:** launchd (low priority — very few users, but free via the same lib).

The shared runtime now uses `kardianos/service` with service name `db-agent`.
`install --service-only` registers and starts an already-enrolled agent without
repeating setup; incomplete DB/key prerequisites are reported by heartbeat, not
treated as backup readiness. Normal installation gates service registration on
the existing diagnostics. Install/start/stop/uninstall are idempotent and require
Administrator/root permissions. Installation captures absolute binary/config
paths and uses Windows automatic startup, systemd enablement, and launchd
`RunAtLoad`; an existing registration is never silently rebound on reinstall.

The service runs as LocalSystem on Windows or root on Unix. Registration remains
a one-time setup exchange, not a runtime worker. Diagnostics, heartbeats, scheduling,
backup/recovery, and signed-update checks run beneath one cancellable lifecycle.
Stop and Windows machine shutdown wait for workers and reverse-order cleanup hooks, with a
15-second stop budget. Hooks get a fresh bounded context for resource cleanup;
backup workers respond to cancellation and join before returning.
Terminal worker failures and update handoff also terminate the service host.
Uninstall stops the service and removes only its registration, preserving config,
pending enrollment, keys, backups, and logs. Service-manager logs are wired;
rotating local file logs and live OS/reboot validation remain pending. Launchd
automatic updates are disabled until the updater supports its restart path.

### Install flow (Windows — the primary target)

The install experience is the highest-risk part of adoption. Target: **under two
minutes, no prior knowledge.**

One-line install:

```powershell
irm https://get.example.com/db-agent.ps1 | iex
```

The script should:

1. Detect architecture, download the correct binary.
2. Place it in `C:\Program Files\db-agent\`.
3. Run an interactive setup wizard:
   - Prompt for the **enrollment token** (copy-pasted from the dashboard).
   - Prompt for DB connection details, defaulting to `127.0.0.1:3306`.
   - Prompt for an *existing* DB admin user/password **once**, used only
     locally, to create the scoped backup user (see below). Never stored, never
     transmitted.
   - Generate the encryption key, display it once, require the user to
     acknowledge they've saved it.
4. Register and start the service.
5. Run `db-agent test` and print a clear success/failure result.
6. Trigger a first backup immediately so the dashboard shows a green state
   within minutes of install (this matters a lot for perceived reliability).

Also ship a **signed `.msi` / `.exe` installer** with the same wizard for users
who won't touch PowerShell. A meaningful share of this audience will prefer
double-click-and-next. Code-sign the binary — an unsigned executable that
touches a database will trip SmartScreen and scare off users.

### Scoped database user creation

The installer should create a least-privilege user automatically rather than
asking the user to understand GRANT syntax:

```sql
CREATE USER 'db_agent_backup'@'localhost' IDENTIFIED BY '<generated>';
GRANT SELECT, SHOW VIEW, EVENT, TRIGGER
  ON `<confirmed_database>`.* TO 'db_agent_backup'@'localhost';
-- MySQL 8.0.20+ routine visibility:
GRANT SHOW_ROUTINE ON *.* TO 'db_agent_backup'@'localhost';
```

Repeat the schema grant only for the confirmed application databases. For
database-level GRANT patterns, escape `_` and `%` in schema names so they cannot
grant access to similarly named databases. Do not use `CREATE USER IF NOT EXISTS`
or reset the password of a pre-existing account when retrying setup.

The shared local-TCP wizard also creates a `db_agent_backup` identity for the
selected loopback IP (`127.0.0.1` or `::1`) with the same password and grants.
Linux MariaDB and MySQL with name resolution disabled do not necessarily map
local TCP to `@localhost`. `localhost` input uses IPv4 TCP; no wildcard-host or
remote-host account is created. Both identities are displayed for approval and
only identities confirmed created by the current attempt are removed on failure.

For MariaDB and MySQL 5.7, use `SELECT ON mysql.proc` for routine visibility instead;
MariaDB 11.3+ also supports global `SHOW CREATE ROUTINE`. Physical backups need
a separate, version-specific privilege and filesystem check. The initial
logical path must not request global `SELECT`, `PROCESS`, table locks, or
physical-backup privileges when narrower grants suffice. `CREATE USER`/`GRANT`
take effect without `FLUSH PRIVILEGES`.

Print exactly what it's about to do and ask for confirmation. If the user
declines, fall back to asking them to supply credentials manually and document
the required grants.

---

## Auto-update

Non-negotiable for a fleet of Windows machines you cannot log into.

- Agent checks for a newer version on a schedule (e.g. daily) via the control
  plane.
- Downloads, verifies signature/checksum, replaces the binary, restarts the
  service.
- Must be **safe to interrupt** — never leave a half-written binary. Download to
  a temp path, verify, then atomically swap.
- Respect a config flag `auto_update: true|false` for users who want to pin.
- The release feed contract, offline signing steps, and restart helper are
  documented in `UPDATE.md`. The public signing key is pinned in release builds.

---

## Technical requirements

- **Go**, single static binary, no runtime dependencies.
- Cross-compile targets: `windows/amd64` (primary), `linux/amd64`,
  `linux/arm64`, `darwin/arm64`.
- No CGO — keep cross-compilation trivial.
- Structured logging to a local rotating log file, plus stdout when run
  interactively. Log location alongside config.
- Graceful shutdown: finish or cleanly abort an in-flight backup on service stop.
- Retry with exponential backoff on all network calls. A transient API outage
  must not lose a backup — retry the upload, and if it ultimately fails, report
  the failure on the next successful heartbeat.
- Config reload without reinstall (re-read on service restart is acceptable for
  MVP).

## Suggested libraries

- Service management: `github.com/kardianos/service`
- Scheduling: `github.com/robfig/cron/v3`
- MySQL driver: `github.com/go-sql-driver/mysql` (for `test` / version detection;
  the actual dump shells out to `mariabackup`/`mysqldump`)
- Compression: `github.com/klauspost/compress/zstd`
- Config: `gopkg.in/yaml.v3`
- Logging: `log/slog` (stdlib)

---

## Error handling — the cases that will actually happen

Handle these explicitly with human-readable messages, not stack traces:

| Case | Expected behaviour |
|---|---|
| Invalid / revoked token | Fail install loudly; if at runtime, stop backing up, heartbeat with error, surface in dashboard |
| DB unreachable | Retry with backoff; report `failing` after N consecutive failures |
| `mysqldump`/`mariabackup` not found | Detect at install, tell the user exactly what to install |
| Disk full during dump | Abort cleanly, do not fill the disk and take the game server down |
| Upload fails mid-transfer | Retry; if the presigned URL expired, request a fresh one |
| Control plane unreachable | Keep backing up locally if possible; queue status reports; never block the backup on the API being up |
| Two agents pointed at one Database object | Control plane should reject/warn; agent should surface it |

That "disk full" row matters more than it looks. Filling a game server's disk
would be worse than not backing up at all.

---

## Open source

The agent is intended to be open-sourced (MIT or Apache 2.0). Write it as if
strangers will read it:

- No hardcoded internal URLs — everything configurable, with sensible defaults.
- No secrets, no internal-only assumptions in the code.
- Clear README: what it does, how to install, how to restore, what data leaves
  the machine (be explicit — this is the trust argument).
- The control plane API stays closed. Only the agent is public.

---

## Definition of done for the MVP

- [ ] Installs on Windows Server in under two minutes via one-line script
- [ ] Registers against a Database object using a scoped token
- [ ] Creates a least-privilege DB user automatically
- [ ] Generates and displays an encryption key exactly once
- [ ] Runs as a service, survives reboot
- [ ] Takes a scheduled backup without noticeably impacting the game server
- [ ] Compresses, encrypts, uploads to object storage via presigned URL
- [ ] Heartbeats every 60s with status
- [ ] Reports backup success/failure/size/duration
- [ ] `db-agent test` gives a clear pass/fail diagnosis
- [ ] `db-agent restore` produces a usable dump file
- [ ] Auto-updates safely
- [ ] Same binary works on Linux via systemd
