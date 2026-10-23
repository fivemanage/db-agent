# FiveManage Database Agent

`db-agent` backs up your server's MySQL or MariaDB database to FiveManage.
It runs as a Windows service or Linux systemd service, with scheduled backups
and backup history in the dashboard. You can also run backups from the command line.

Backups are compressed and encrypted with AES-256-GCM before upload. Database
passwords and encryption keys stay on your server. The agent uses a dedicated
read-only database account and outbound connections; you don't need to open any
inbound ports.

You can recover backups as SQL files, even on a replacement server without a
FiveManage connection, using a downloaded backup and your saved recovery key.

The agent is still in development. Public installers and testing on target
operating systems are pending. Dashboard restore is not yet ready for production use.

**Keep a recovery-key copy outside this server. Without the original key,
your backups cannot be decrypted. FiveManage cannot recover or replace it.**

## Requirements

| Component | Requirement |
| --- | --- |
| Operating system | Windows amd64, or Linux amd64/arm64 with systemd |
| Database | Local MySQL 5.7+ or MariaDB 10.3+ |
| Dump tools | Matching `mysqldump` or `mariadb-dump` on the system/service PATH |
| Permissions | Administrator on Windows; root for Linux service installation and operation |
| Network | Outbound access to the FiveManage API, backup storage, and release downloads |
| Disk space | Space for the encrypted backup, plus at least 256 MiB free reserve |

Install dump tools from the same database family and major version; the client
minor version must be at least the server's. Backups take a consistent snapshot
of InnoDB data, but that guarantee doesn't cover nontransactional tables or schema
changes made during a backup.

## Set up the agent

Create a database entry in the FiveManage dashboard and copy its one-time setup
token. Put the agent executable where you intend to keep it before running
`install`: the service will use that path each time it starts.

### Windows

Open **64-bit PowerShell as Administrator**. With `db-agent.exe` placed in
`C:\Program Files\db-agent`:

```powershell
& 'C:\Program Files\db-agent\db-agent.exe' install
& 'C:\Program Files\db-agent\db-agent.exe' status
```

### Linux

With the binary placed at `/opt/db-agent/db-agent` in a root-owned, protected directory:

```sh
sudo /opt/db-agent/db-agent install
sudo /opt/db-agent/db-agent status
```

Release builds can include the FiveManage API URL. Development builds require
`install --api-url <control-plane-base-url>` on first enrollment.

### What setup asks for

Turn off terminal recording and PowerShell transcripts before setup, since it
displays your recovery key.

1. Enter the dashboard setup token.
2. Paste your database connection string or enter the details manually. Setup
   can create a backup account with your approval or use an existing one.
   Database administrator credentials are never saved.
3. Save the recovery key in a password manager or offline storage, then confirm
   your copy. The key is shown only once.

Setup checks database access, dump tools, the recovery key, and API connectivity
before installing and starting the service.

Supported connection-string forms include:

```text
mysql://user:password@127.0.0.1:3306/game_database
server=127.0.0.1;port=3306;database=game_database;userid=user;password=secret
```

Percent-encode special characters in URI values. Remote database hosts and
unsupported connection options are rejected.
Do not save root or your database administrator account as the backup login.

The optional **Enable dashboard restores? [y/N]** step creates a separate restore
account. Review its permissions before authorizing it: this account can manage
database users, while the backup account remains read-only. You can skip this
step and enable restores later by rerunning `install`.

If setup is interrupted, rerun `install` with the same config. It keeps your
registration, database settings, and recovery key. Do not delete a key or pending
enrollment file to retry. An existing key is never regenerated or redisplayed;
if its initial display was lost, an authorized local administrator must save a
copy of the protected key file before confirming it.

## Backups and status

On first startup, the service attempts a backup once its checks pass. Setup
doesn't wait for the upload to finish. Check the result in the dashboard or with
`status`; a running service alone doesn't mean you have a completed backup.

Examples below use `db-agent` on your PATH. On Windows, use `db-agent.exe`; on
Linux, run these commands as root. Add `--config <path>` if your install
uses a nondefault config.

```sh
db-agent test          # Check the setup
db-agent status        # Show the last backup result and next scheduled run
db-agent backup --now  # Run a backup and wait for the upload to complete
```

Backups run daily at **4am in the machine's local timezone** by default. The
`backup.schedule` setting accepts a five-field cron expression. Local schedule
or timezone changes require a service restart; accepted dashboard schedule changes
are picked up automatically. Overlapping runs are skipped, not queued, and missed
scheduled runs are not replayed after downtime.

Only one backup, transfer recovery, or dashboard restore can run at a time.
Interrupted transfers are resolved before a new backup starts. Encrypted backups
are temporarily stored on disk before upload, with a **1 GiB limit** and at least
**256 MiB of free disk space** left over.

The agent reports connectivity every 60 seconds. The dashboard marks it stale
after three minutes without a heartbeat. Connectivity and backup success are
separate: `idle` does not mean a backup completed. CLI `status` shows recorded
state, **not live service health**; after a crash, its next-run time may be stale.

## Recover a backup

### Recover SQL from the command line

Download and decrypt a backup using the installed agent's local key:

```sh
db-agent restore --backup-id <id> --output ./recovered.sql
```

Or recover an already-downloaded encrypted file:

```sh
db-agent restore --file ./backup.dbak --output ./recovered.sql
```

On a replacement machine, use the encrypted backup and your original saved key
file. No enrollment, config, or API connection is needed:

```sh
db-agent restore --file ./backup.dbak --key-file ./recovery.key --output ./recovered.sql
```

Never put key contents in command-line arguments. Key files must be private:
`0600` on Unix, or protected System/Administrators-only ACLs on Windows.

**These commands recover SQL; they do not import it or change a database.**
The agent verifies and decompresses the entire backup before saving the SQL file.
Existing files are never overwritten. Defaults allow two hours, 1 GiB encrypted input,
and 16 GiB SQL output, with a 256 MiB free-disk reserve.

Recovered SQL contains the original database names, including `CREATE DATABASE`
and `USE`. Import only into an isolated restore server. Selecting a different
default database or replacing names in text is not a safe production restore.
Protect the recovered SQL and remove it when finished.

### Dashboard restore

If you enabled restore access during setup, the agent can restore a backup from
the dashboard into a new, isolated database using its local key. Your production
database stays untouched. You decide when to point your application at the
restored database.

Dashboard restore still needs production-readiness testing. Only supported table
and view definitions can be imported. Routines, triggers, events, or other
unsupported SQL cause the whole restore to stop before any database writes.
Dashboard restore permits up to 8 GiB each of
encrypted input and uncompressed SQL, with additional statement/object limits.
These limits do not raise the separate 1 GiB backup-upload limit.

## Configuration and service management

| Data | Windows default | Linux default |
| --- | --- | --- |
| Config | `C:\ProgramData\db-agent\config.yaml` | `/etc/db-agent/config.yaml` |
| Recovery key | `encryption.key` beside config | `encryption.key` beside config |
| Runtime logs | Event Viewer, source `db-agent` | `journalctl -u db-agent` |

Choose a config with `--config <path>` or `DB_AGENT_CONFIG`. Installation saves
its absolute path for the service. Config and keys contain sensitive material:
keep Unix files at `0600` and Windows files restricted to System/Administrators.
Never paste them into chat, support tickets, or the dashboard.

| Command | Purpose |
| --- | --- |
| `db-agent configure` | Change or retry the local database connection |
| `db-agent configure --key` | Resume recovery-key setup and confirmation |
| `db-agent configure --restore-access` | Authorize an existing dedicated restore account |
| `db-agent agent` | Run in the foreground instead of as an installed service |
| `db-agent stop` / `db-agent start` | Control the installed service |
| `db-agent uninstall` | Stop and remove service registration; preserve config, keys, and state |
| `db-agent update check` / `db-agent update now` | Check for or apply a signed update |

The service is named `db-agent`, starts automatically on boot, and runs as
LocalSystem on Windows or root on Linux. If you've already registered the agent,
`install --service-only` installs the service without rerunning setup. To change
the executable or config path used by an existing service, uninstall and reinstall
the service.

Release builds check for signed updates at startup and every 24 hours when
`auto_update` is enabled. The agent verifies signatures and checksums before
replacement, with rollback if restart fails. Development builds skip automatic
checks.

## Build and development

```sh
make build      # Current platform binary in bin/
make build-all  # Windows amd64 and Linux amd64/arm64 binaries in bin/
make check      # go test ./... and go vet ./...
```

Builds default to version `dev`; override with `make build VERSION=0.1.0`.
These are unsigned development builds, not production releases. Use a permanent
binary path for service installation, not `go run`.

For a local dashboard, use `install --api-url http://localhost:3000`. Supply the
base URL without `/api` or `/api/v1`; the agent appends its endpoint paths.
The matching control-plane API lives in the separate `fivemanage` repository.
