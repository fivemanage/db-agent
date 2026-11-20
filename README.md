# FiveManage Database Agent

`db-agent` backs up your server's MySQL or MariaDB database to FiveManage.
It runs as a Windows service or Linux systemd service, with scheduled backups
and backup history in the dashboard. You can also run backups from the command line.

Backups are compressed and encrypted with AES-256-GCM before upload. Database
passwords stay on your server. The agent uses a dedicated read-only database
account and outbound connections; you don't need to open any inbound ports.

You can recover backups as SQL files, even on a replacement server without a
FiveManage connection, using a downloaded backup and your saved recovery key.

The agent is still in development. Public installers and testing on target
operating systems are pending. Dashboard restore is not yet ready for production use.

**You need the original recovery key.** FiveManage stores an encrypted copy by
default for new databases unless you opt out. An organization owner can download
that copy, even if the original server is gone. Holding the key allows FiveManage
to decrypt your backups. If FiveManage doesn't have a saved copy, keep your own
copy off the server.

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

The FiveManage Windows installer adds `C:\Program Files\db-agent` to the system
PATH, so after it finishes you can run `db-agent status` from any folder.
PowerShell windows that were already open before installing need to be reopened
first. If you placed the executable yourself, add its folder to PATH in **System
Properties → Environment Variables → System variables → Path**, or keep using the
full path as shown above.

### Linux

With the binary placed at `/opt/db-agent/db-agent` in a root-owned, protected directory:

```sh
sudo /opt/db-agent/db-agent install
sudo /opt/db-agent/db-agent status
```

The FiveManage Linux installer creates `/usr/local/bin/db-agent`, pointing to the
installed binary, so you can run `sudo db-agent status` from any folder on systems
with `/usr/local/bin` on PATH. It won't overwrite an existing command. If you placed
the binary yourself, keep using the full path or create the link manually:

```sh
sudo ln -s /opt/db-agent/db-agent /usr/local/bin/db-agent
```

Release builds can include the FiveManage API URL. Development builds require
`install --api-url <control-plane-base-url>` on first enrollment.

### What setup asks for

1. Enter the dashboard setup token. The agent registers and starts its service
   right away, so the dashboard can see it even if you close the terminal later.
2. Paste your database connection string or enter the details manually. If you
   connect with an admin login, setup offers to create a read-only backup user
   for you. Otherwise, use an existing backup login. Database logins are saved on
   this server only, and database administrator credentials are never saved.
3. Optionally create a restore user for dashboard restores.

Then go back to the database page in the dashboard to finish setup. If you
enabled the managed recovery copy, wait for the page to say FiveManage saved it.
Otherwise, follow the instructions to save a copy in a password manager or other
storage off this server and confirm you saved it. Select **Finish setup** to run
the checks and start the first backup. If a check fails, fix it on the server
and try again.

For an existing database, an organization owner can sign in again and select
**Store a copy with FiveManage** in the database's **Settings** tab. Keep the
original server and key available until the updated agent saves the copy.
This doesn't change the key or affect existing backups.

To finish in the terminal instead, run `configure --key`. It asks you to paste
the key from the file to confirm your copy.

After you generate a setup token, the dashboard's copyable install command can
pass it directly to setup, so you don't need to paste it a second time. That
command contains a secret and may remain in your clipboard or shell history;
it can also appear in process arguments or system/sudo logs. Don't share it.
The installers accept `--auth '<setup-token>'` on Linux or
`-AuthToken '<setup-token>'` on Windows. They pass it internally through a temporary
environment variable, not through agent arguments. The agent clears that variable
before starting child processes and saves only the issued agent credential,
never the setup token. Omitting the argument keeps the hidden token prompt.

Supported connection-string forms include:

```text
mysql://user:password@127.0.0.1:3306/game_database?charset=utf8mb4&connectionLimit=50
server=127.0.0.1;port=3306;database=game_database;userid=user;password=secret
```

Percent-encode special characters in URI values. Setup ignores `connectionLimit`
in MySQL URLs because it only controls your game's connection pool.
Add `ssl=true` to require verified TLS for setup, backups, and dashboard restores.
Setup asks for the absolute path to a PEM CA certificate file. Keep that file
readable by the agent service and writable only by an administrator. The database
certificate must include the loopback IP as an IP subject alternative name
(`127.0.0.1` for `localhost`, or `::1`).
If verification fails, the agent won't fall back to an unencrypted connection.
Use `ssl=false` to explicitly disable TLS. Remote database hosts, sockets, and
other unsupported connection options are rejected.
Do not save root or your database administrator account as the backup login.

Setup also asks **Create a restore user?** (default no) so you can restore backups
from the dashboard. The restore user is separate from the read-only backup user
and can create databases and database users, so keep this machine's database
admin access protected. You can skip this step and enable restores later by
running `configure --restore-access` with the same config.

If setup is interrupted, rerun `install` with the same config, or run
`configure` to manage the saved database connection or dashboard restore access.
If no database connection is saved, `configure` starts connection setup.
Both preserve your registration and recovery key. Database settings change only
when you confirm a new connection. Do not delete a key or pending enrollment
file to retry. An existing key is never regenerated.

## Backups and status

The service attempts the first backup once setup is finished and its checks
pass. Setup doesn't wait for the upload to finish. Check the result in the
dashboard or with `status`; a running service alone doesn't mean you have a
completed backup.

Examples below use `db-agent` on your PATH. On Windows, run them in PowerShell
as Administrator; on Linux, run them as root. Add `--config <path>` if your install
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

If the original server is gone, download a retained encrypted backup from
**Backups**. If FiveManage has a saved copy of your key, sign in again as the
organization owner and select **Download recovery key** in the database's
**Settings** tab.
Otherwise use the original key file you saved off the server.

On a replacement machine, restrict the key file before using it. Don't put the
key contents in a command.

On Linux or macOS:

```sh
chmod 600 ./recovery.key
db-agent restore --file ./backup.dbak --key-file ./recovery.key --output ./recovered.sql
```

On Windows, open PowerShell as Administrator in the download folder. Use the
actual filenames if your browser renamed the files:

```powershell
$path = (Resolve-Path -LiteralPath '.\recovery.key').Path
$acl = Get-Acl -LiteralPath $path
$acl.SetSecurityDescriptorSddlForm('D:P(A;;FA;;;SY)(A;;FA;;;BA)', [System.Security.AccessControl.AccessControlSections]::Access)
Set-Acl -LiteralPath $path -AclObject $acl
& 'C:\Program Files\db-agent\db-agent.exe' restore --file .\backup.dbak --key-file $path --output .\recovered.sql
```

Offline recovery needs only the executable and the two files. It doesn't need
enrollment, config, or an API connection.

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

To enable or repair access later, run `db-agent configure --restore-access`.
It reuses your saved database connection. You can check and re-enable a saved
restore account, create one if none is saved, or enter an existing dedicated
account's credentials. Creating an account asks for a one-time admin login,
which is never saved. Backup access and your recovery key stay unchanged.
Use the same `--config` path as the agent if you chose a nondefault config.

Dashboard restore still needs production-readiness testing. It imports supported
tables, views, and row triggers into the new database. Views and triggers use
destination-only references and a restricted local definer, never the original
database account. Triggers are created after loading data so they don't run again
on the backed-up rows.

Supported triggers can assign `NEW` columns before inserts/updates, insert/update/
delete rows in other restored tables, and use `BEGIN` blocks and `IF` branches.
Stored routine calls, dynamic SQL, cross-database references, routines, events,
and other unsupported SQL stop the whole restore before any database writes.
View support remains limited to validated projections, joins, predicates, and
allowlisted scalar functions. Aggregates and subqueries are not supported.

Restore accounts created before trigger support need a local administrator to
add `TRIGGER` to their existing restore-namespace grant, with grant option, only
when restoring a backup that contains triggers. Table/view-only restores keep
working without this extra permission.
After upgrading, `db-agent update check` and `db-agent update now` show the exact
SQL for the configured account if it needs this grant. Agent startup prints the
same notice to the service logs, including after automatic updates. These checks
are read-only; the agent never runs the displayed grant.
Updates preserve existing database users, configuration, and encryption keys.
Do not delete them to update the agent.
Keep this permission separate from the backup account. If binary logging is on,
trigger restore also requires `log_bin_trust_function_creators` enabled. The
agent checks this before creating accounts or importing and never changes it.

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
| `db-agent configure` | Manage the saved database connection or dashboard restore access |
| `db-agent configure --key` | Resume recovery-key setup and confirmation |
| `db-agent configure --restore-access` | Enable, check, or repair dashboard restore access |
| `db-agent agent` | Run in the foreground instead of as an installed service |
| `db-agent stop` / `db-agent start` | Control the installed service |
| `db-agent uninstall` | Stop and remove service registration; preserve config, keys, and state |
| `db-agent update check` / `db-agent update now` | Check for or apply a signed update |

The service is named `db-agent`, starts automatically on boot, and runs as
LocalSystem on Windows or root on Linux. If you've already registered the agent,
`install --service-only` installs the service without rerunning setup. To change
the executable or config path used by an existing service, uninstall and reinstall
the service.

Release builds check GitHub Releases at startup and every 24 hours when
`auto_update` is enabled. Stable installations use the published Latest stable
release; prerelease installations use the newest published staging candidate.
Drafts are never selected. No GitHub login or token is needed. The agent verifies
signatures and checksums before replacement, with rollback if restart fails.
Development builds skip automatic checks. A local `update_url` can override GitHub
with an HTTPS signed-manifest feed; `{os}` and `{arch}` expand to your platform.

Stable installations accept only stable updates. Prerelease builds use a separate
test channel and should run on isolated test systems, not production servers.

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
