# FiveManage Database Agent (`db-agent`)

Automatic, encrypted MySQL and MariaDB backups for your game server.

`db-agent` runs quietly as a Windows Service or Linux `systemd` service, takes low-impact backups of your local database, compresses and encrypts them with AES-256-GCM before upload, and reports status to your FiveManage dashboard.

- **Outbound-only:** No inbound ports to open; works behind NAT and dynamic IPs.
- **Zero-lock InnoDB dumps:** Uses `--single-transaction`, low process priority, and I/O throttling to avoid game-server lag.
- **Least-privilege access:** Automatically creates a dedicated read-only `db_agent_backup` user; database credentials never leave your server.
- **Encrypted before upload:** Backups are compressed (`zstd` or `gzip`) and encrypted locally before leaving your machine.
- **Safe auto-updates:** Signed releases verify Ed25519 signatures and SHA-256 checksums before replacing the binary, with automatic rollback.

---

## Contents

- [Requirements](#requirements)
- [Quickstart](#quickstart)
- [Commands](#commands)
- [Backups & schedules](#backups--schedules)
- [Restoring a backup](#restoring-a-backup)
- [Updating `db-agent`](#updating-db-agent)
- [Security & privacy](#security--privacy)
- [Files & logs](#files--logs)
- [Troubleshooting](#troubleshooting)
- [Building from source](#building-from-source)

---

## Requirements

| Requirement | Supported |
| --- | --- |
| **OS** | Windows (`amd64`), Linux (`amd64` / `arm64` with `systemd`) |
| **Database** | Local MySQL 5.7+ or MariaDB 10.3+ (loopback TCP) |
| **Dump client** | Matching `mysqldump` or `mariadb-dump` (same family and major version, minor >= server) |
| **Permissions** | Administrator on Windows; `root` / `sudo` on Linux |
| **Network** | Outbound HTTPS to FiveManage, backup storage, and GitHub Releases |
| **Disk space** | Free space for temporary transfer chunks plus at least 256 MiB reserve |

---

## Quickstart

### 1. Create a database in FiveManage

Open your team's **Databases** page in the FiveManage dashboard, create a new database, and copy the install command (or one-time setup token).

### 2. Run the installer

**Windows** — open **64-bit PowerShell as Administrator**:

```powershell
irm https://fivemanage.com/install/database.ps1 | iex
```

**Linux** (`amd64` / `arm64` with `systemd`):

```sh
curl -fsSL https://fivemanage.com/install/database.sh | sudo bash
```

> **Tip:** The dashboard also gives you a one-liner that pre-fills your one-time setup token (`-AuthToken '<token>'` on Windows or `--auth '<token>'` on Linux). Treat that command as a secret — don't share it or record your screen while running it.

### 3. Follow the setup prompts

1. **Connect to FiveManage:** Paste your one-time setup token (skipped if passed via the dashboard command). The agent registers and starts its background service immediately.
2. **Connect your database:** Paste your FiveM `mysql_connection_string` or enter host/port/user/password manually:
   ```text
   mysql://user:password@127.0.0.1:3306/es_extended?charset=utf8mb4&connectionLimit=50
   mysql://root@localhost/qbcore
   server=127.0.0.1;port=3306;database=es_extended;userid=root;password=secret
   ```
   - If you connect with an admin account (including a passwordless local `root`), `db-agent` offers to create a dedicated read-only `db_agent_backup` user with a strong random password. Admin credentials are never saved.
   - `connectionLimit` is safely ignored; `ssl=true` requires verified TLS with a local PEM CA file.
3. **Dashboard restores (optional):** Choose whether to create a separate `db_agent_restore` user so you can restore backups into isolated databases directly from the dashboard. You can skip this and enable it anytime later with `db-agent configure --restore-access`.
4. **Dump tool check:** `db-agent` checks for a compatible `mariadb-dump` or `mysqldump`. On Windows, it can locate existing installations in `Program Files` and offer to add them to System `PATH`; on Linux, it can offer to install `mariadb-client` or `mysql-client`.

### 4. Finish in the dashboard

Return to your database page in FiveManage, confirm your recovery key setup, and click **Finish setup**. The agent runs its checks and immediately starts your first backup.

---

## Commands

Run commands in **PowerShell as Administrator** on Windows, or with **`sudo`** on Linux. Pass `--config <path>` if you use a custom config location.

```sh
db-agent status                       # Show last backup result and next scheduled run
db-agent test                         # Run full self-diagnostics (DB, grants, dump tool, key, API)
db-agent backup --now                 # Run a backup immediately and wait for upload
db-agent configure                    # Change saved database settings or restore access
db-agent configure --restore-access   # Enable, check, or repair dashboard restore access
db-agent configure --key              # Confirm your recovery key in the terminal
db-agent update check                 # Check whether a signed update is available
db-agent update now                   # Download, verify, and install the latest signed update
db-agent start | stop                 # Start or stop the background service
db-agent uninstall                    # Remove the service (keeps config.yaml and encryption.key)
db-agent version                      # Print the installed version
```

> **Windows PATH note:** The Windows installer adds `C:\Program Files\db-agent` to your System `PATH`. Any PowerShell windows that were already open need to be reopened before `db-agent` works without the full path.

---

## Backups & schedules

- **Managing settings:** Change your backup schedule (Hourly, Every 6 hours, Every 12 hours, Daily, or custom 5-field cron), retention days, compression (`zstd` / `gzip`), I/O throttling, and auto-updates from your database's **Settings** tab in FiveManage. Changes apply automatically on the agent's next heartbeat (within 60 seconds) without restarting.
- **Local time:** Cron expressions run in the server's local timezone.
- **No overlap:** Only one backup, transfer recovery, or restore runs at a time. Overlapping scheduled ticks are skipped rather than queued.
- **Large databases:** Backups stream through compression and encryption and use multipart uploads for larger databases while keeping at least 256 MiB of free disk headroom.
- **Consistency note:** Logical dumps use `--single-transaction`, which gives a consistent snapshot for InnoDB tables without locking them. Nontransactional tables and concurrent schema changes (`ALTER TABLE`) during a dump are not covered by the snapshot.

---

## Restoring a backup

### Option 1: Dashboard restore (isolated database)

If you enabled restore access during setup (or via `db-agent configure --restore-access`), open **Backups** in the FiveManage dashboard and click **Restore** on any completed backup.

- The agent downloads and decrypts the backup automatically using its local key.
- It imports into a **brand-new isolated database** (`fm_restore_*`) so your live game database is never overwritten.
- Once verified, inspect the restored database and switch your server over when you're ready.
- Supports standard InnoDB tables, views, and row triggers (with destination-confined references and restricted definers). Backups containing unsupported SQL constructs (such as stored routines or events) fail safely before writing anything — use CLI SQL recovery below for those.

### Option 2: CLI & offline SQL recovery

Recover a verified `.sql` dump file without touching any database:

```sh
# Download and decrypt using the installed agent's local key
db-agent restore --backup-id <id> --output ./recovered.sql

# Decrypt an already-downloaded .dbak file using the installed agent's local key
db-agent restore --file ./backup.dbak --output ./recovered.sql
```

#### Recovering on a replacement server (offline)

If your original server is gone, download a retained `.dbak` file from **Backups** in the dashboard and get your recovery key (from your own backup, or by clicking **Download recovery key** in **Settings** as the organization owner if you enabled FiveManage managed recovery keys).

Offline recovery needs only the `db-agent` executable, the `.dbak` file, and the key file — no enrollment or internet connection required. First lock down the key file's permissions:

**Linux / macOS:**

```sh
chmod 600 ./recovery.key
db-agent restore --file ./backup.dbak --key-file ./recovery.key --output ./recovered.sql
```

**Windows (PowerShell as Administrator in the folder with both files):**

```powershell
$path = (Resolve-Path -LiteralPath '.\recovery.key').Path
$acl = Get-Acl -LiteralPath $path
$acl.SetSecurityDescriptorSddlForm('D:P(A;;FA;;;SY)(A;;FA;;;BA)', [System.Security.AccessControl.AccessControlSections]::Access)
Set-Acl -LiteralPath $path -AclObject $acl
& 'C:\Program Files\db-agent\db-agent.exe' restore --file .\backup.dbak --key-file $path --output .\recovered.sql
```

> **Safety reminder:** `db-agent restore` produces a `.sql` file — it does **not** import it into your database. Because dumps include `CREATE DATABASE` and `USE` statements, always import recovered SQL into an isolated test/restore server first, and delete the plaintext `.sql` file when you're done.

---

## Updating `db-agent`

When **Automatic agent updates** (`auto_update: true`) is enabled, `db-agent` checks GitHub Releases on startup and every 24 hours. Before replacing the executable, it verifies the Ed25519 signature and SHA-256 checksum, swaps the binary atomically, restarts the service, and rolls back automatically if startup fails.

To check or update manually at any time:

```sh
db-agent update check
db-agent update now
```

Updates never touch your `config.yaml`, `encryption.key`, or database users.

<details>
<summary>Upgrading from v0.1.0 on Windows (before automatic GitHub release discovery)</summary>

Run these three commands in **64-bit PowerShell as Administrator** — your `config.yaml` and `encryption.key` in `C:\ProgramData\db-agent\` stay untouched:

```powershell
db-agent stop
Rename-Item 'C:\Program Files\db-agent\db-agent.exe' 'db-agent.exe.v0.1.0.bak'
& ([scriptblock]::Create((irm https://fivemanage.com/install/database.ps1))) -ServiceOnly
```

</details>

---

## Security & privacy

### What leaves your server

| Stays on your server | Sent to FiveManage |
| --- | --- |
| Database passwords & connection strings | Encrypted backup files (`.dbak`, AES-256-GCM) |
| Plaintext SQL dumps | Agent metadata (OS, arch, version, hostname, DB engine/version) |
| Local `config.yaml` and `db_agent_backup` credentials | Heartbeat status, backup size/duration/checksum, classified error messages |
| Local `encryption.key` (unless managed copy is enabled) | Encrypted recovery key copy *(only if enabled; see below)* |

### Recovery key options

- **Managed copy (default for new databases):** The original key stays on your server and is used for all normal backups and restores. `db-agent` also uploads an encrypted copy to FiveManage so an organization owner (after a fresh sign-in) can download it even if the original server is lost. Holding a copy means FiveManage can decrypt your backups; you can opt out when creating a database.
- **Self-managed (opt-out):** The key never leaves your server. Save a copy of `encryption.key` in a password manager or off-server storage — if both the server and your copy are lost, backups cannot be decrypted.
- For existing self-managed databases, an organization owner can opt in later from **Settings → Store a copy with FiveManage** without changing the key or affecting existing backups.

---

## Files & logs

| Item | Windows | Linux |
| --- | --- | --- |
| **Executable** | `C:\Program Files\db-agent\db-agent.exe` | `/opt/db-agent/db-agent` (symlinked at `/usr/local/bin/db-agent`) |
| **Config** | `C:\ProgramData\db-agent\config.yaml` | `/etc/db-agent/config.yaml` |
| **Recovery key** | `C:\ProgramData\db-agent\encryption.key` | `/etc/db-agent/encryption.key` |
| **Service name** | `db-agent` (LocalSystem) | `db-agent.service` (`systemd`, root) |
| **Runtime logs** | Event Viewer → Application (source `db-agent`) | `journalctl -u db-agent` |

Keep `config.yaml` and `encryption.key` restricted (`0600` on Linux, SYSTEM/Administrators-only on Windows) and never paste them into support tickets or chat.

---

## Troubleshooting

| Issue | What to do |
| --- | --- |
| **`mysqldump` / `mariadb-dump` not found** | Install the client tools matching your server's family and major version. On Windows, add the `bin` folder (e.g. `C:\Program Files\MariaDB 11.4\bin`) to **System** `PATH`, then run `db-agent test` (or restart the service with `db-agent stop` && `db-agent start`). |
| **Setup interrupted partway through** | Rerun `db-agent install` (or `db-agent configure` if already registered). Do **not** delete `config.yaml` or `encryption.key` — existing enrollment and keys are preserved automatically. |
| **Agent shows offline / stale in dashboard** | Check `db-agent status` and `db-agent test`. On Windows, ensure the `db-agent` service is running (`db-agent start`). Check outbound HTTPS firewall rules. |
| **Cannot read `recovery.key` on Windows** | Downloaded files inherit your user's ACL. Run the `Set-Acl` snippet in [Recovering on a replacement server](#recovering-on-a-replacement-server-offline) in Admin PowerShell so only SYSTEM and Administrators have access. |
| **Permissions error running `db-agent` on Windows** | Open **64-bit** PowerShell using **Run as administrator**. |
| **Reconnecting a machine to a different database** | Run `db-agent uninstall` (this keeps your old config and key safe), move/archive `C:\ProgramData\db-agent` or `/etc/db-agent` elsewhere (keep `encryption.key` if you still need old `.dbak` files), then run the installer with the new setup token. |

---

## Building from source

Requires Go (see `.go-version`). Development builds default to version `dev` and skip automatic update checks.

```sh
make build      # Build for the current platform into bin/
make build-all  # Build static binaries for windows/amd64, linux/amd64, linux/arm64
make check      # Run go test ./... and go vet ./...
```

For local control-plane development, pass `--api-url` on first install (base URL without `/api/v1`):

```sh
./bin/db-agent install --api-url http://localhost:3000
```
