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
  # If empty, back up all databases the user can see.
  # Most users will have one game database.
  databases: ["es_extended"]

backup:
  schedule: "0 4 * * *"              # cron expression, default 4am local
  retention_days: 7                  # informational; enforced control-plane side
  compression: "zstd"                # zstd | gzip
  throttle_io: true                  # rate-limit to avoid impacting the server

encryption:
  # Key is generated at install time and shown ONCE to the user.
  # Agent stores it locally. Control plane never sees it.
  key_file: "C:\\ProgramData\\db-agent\\encryption.key"
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

Database driver and version are reported later, after local DB setup. Response
confirms the binding, returns the agent credential, and may return
server-side config overrides (e.g. schedule). The installer persists only the
agent credential. If the enrollment token is invalid, the agent should fail loudly with a
clear message — this is the #1 install-time error and the message needs to be
human-readable, not a stack trace.

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

### 3. Backup run

Triggered by schedule, or on demand (see "Manual trigger" below).

Steps:

1. Report `status: backing_up` via heartbeat.
2. Dump the database:
   - **Preferred:** `mariabackup` / `xtrabackup` if available on PATH (physical
     backup — much lower load, supports incremental later).
   - **Fallback:** `mysqldump` with `--single-transaction --quick
     --routines --triggers --events`.
   - Detect which is available at startup and log which one will be used.
3. Stream the dump through compression (zstd default) and then through
   encryption — **do not** write a large plaintext temp file to disk if it can
   be avoided. Stream: `dump → compress → encrypt → upload`.
4. Request a presigned upload URL from the control plane:

```
POST /v1/backups/initiate
Authorization: Bearer <agent credential>

{ "estimated_size_bytes": 348000000, "started_at": "..." }

→ { "backup_id": "...", "upload_url": "https://...", "expires_at": "..." }
```

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

### 4. Manual trigger

The agent should support an on-demand backup:

- CLI: `db-agent backup --now`
- And (optional, if simple) a control-plane-initiated trigger picked up on the
  next heartbeat poll — useful for a "Back up now" button in the dashboard.

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
db-agent version
```

`db-agent test` is worth building early. It turns "it's not working" support
tickets into self-diagnosis.

---

## Service installation

Must run as a background service so it survives reboots and does not require a
logged-in session (essential for VPS users).

- **Windows:** register as a Windows Service. Recommended:
  `github.com/kardianos/service` — handles Windows Service, systemd, and launchd
  from one codebase, which is exactly the cross-platform need here.
- **Linux:** systemd unit.
- **macOS:** launchd (low priority — very few users, but free via the same lib).

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
GRANT SELECT, LOCK TABLES, RELOAD, REPLICATION CLIENT, PROCESS,
      SHOW VIEW, EVENT, TRIGGER
  ON *.* TO 'db_agent_backup'@'localhost';
FLUSH PRIVILEGES;
```

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
