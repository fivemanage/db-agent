# Logical dump execution (DEV-496)

`internal/dump.Run` runs the compatible `mysqldump`/`mariadb-dump` selected by
`database.DetectTools`, streams stdout to a caller-owned writer, and removes its
private credential file on success, failure, timeout, or cancellation. It creates
no SQL dump files. `internal/backup` connects it to upload, locking, and
`backup --now`; scheduling remains DEV-498. This is the reusable execution stage.

## Scope and consistency

- Only explicitly selected application databases on `localhost`, `127.0.0.1`,
  or `::1` are allowed. `localhost` always uses IPv4 TCP, not a Unix socket.
- Empty/system-database selections, duplicate names, control characters, and
  option-shaped names beginning with `-` are rejected. Arguments are passed
  without a shell; `--databases --` separates names from options.
- No automatic all-visible/all-databases fallback exists. Discovery is not
  consent to expand the backup scope.
- Required flags are `--single-transaction --quick --routines --triggers
  --events --no-tablespaces --skip-lock-tables --hex-blob
  --default-character-set=utf8mb4`. No `--force`, log flushing, tablespace
  privilege escalation, or global locking is used.
- MySQL also uses `--set-gtid-purged=OFF`: a scoped logical backup must not change
  global GTID state on restore or require additional RELOAD/FLUSH_TABLES grants.
  MySQL 8+ disables column statistics and allows the authentication public-key
  exchange on loopback with `--column-statistics=0 --get-server-public-key`.
  MySQL-only flags are not passed to MariaDB clients.
- A transactional snapshot guarantees row consistency for **InnoDB**, not
  nontransactional tables such as MyISAM or concurrent schema changes. Avoid DDL
  during a dump. Nontransactional workloads need a separately approved backup
  strategy; do not represent this lock-free logical path as a universal snapshot.
- Physical tools remain discovery-only. Executing `mariabackup`/`xtrabackup`,
  their filesystem/privilege checks, stream/archive formats, throttling, and
  restore preparation are deferred, matching `SPEC.md`'s logical-first contract.
  DEV-496's original physical-first/all-visible wording is not implemented.

## Credentials and environment

A fresh `.db-agent-dump-*` directory holds one `client.cnf`. Unix directory/file
permissions are `0700`/`0600`. Windows both directory and file have a protected
System/Administrators-only DACL; run under the intended Administrator/service
account. `Options.TempDir` can select a suitable private local parent; otherwise
the OS temporary directory is used.

The options file contains quoted user/password/loopback host, port, and TCP
protocol. Backslashes, quotes, newlines, carriage returns, tabs, and backspaces
are escaped using MySQL option-file syntax, not Go/SQL string literal syntax.
NUL credentials are rejected. Special-character passwords are tested with both
real client parsers. This small temporary file contains credentials, **not dump
data**, and must never be uploaded or logged.

The first argument is `--defaults-file=<absolute private path>` rather than
`--defaults-extra-file`. Ambient config files must not inject output files,
expanded scope, dangerous flags, or different credentials. MySQL still reads
login paths with `--defaults-file`, so `MYSQL_TEST_LOGIN_FILE` is set to a
nonexistent path inside the private directory (supported by MySQL 5.7+).
Inherited `MYSQL_*`/`MARIADB_*` variables are removed, including `MYSQL_PWD`.
The password is never passed in process arguments or a child environment.

The executor removes only its own credential file and directory; it does not
recursively delete arbitrary temporary trees. Cleanup failures are reported as
failures, not ignored successes. A hard process/machine crash can leave a private
credential directory; automatic stale-file recovery is not implemented here.

## Timeout, throttling, and process lifetime

Local config adds:

```yaml
backup:
  dump_timeout: "2h"   # positive Go duration, e.g. 30m; default 2h
  throttle_io: true    # existing setting, default true
```

`dump_timeout` cannot be changed by dashboard overrides. The orchestrator
passes `Backup.DumpDuration()` and `Backup.ThrottleIO` to `dump.Options`.
When throttling is enabled, stdout is limited to **4 MiB/s** by default;
`Options.BytesPerSecond` permits a different positive rate. Zero uses the default
when throttling is enabled; disabling throttling allows unthrottled output.
Bounded 32 KiB chunks and cancelable waits propagate backpressure to the client
and its database reads. This bounds output rate, not all database CPU/I/O costs.

Unix starts the client in its own process group and lowers it to nice 19; group
termination stops ordinary descendants. Windows uses below-normal priority and
a kill-on-close Job Object. The process is created suspended, assigned to the
job, then resumed, avoiding a race where a child could escape before assignment.
Only explicitly duplicated stdin/stdout/stderr handles are inherited. Job setup
or priority setup failures stop execution rather than silently losing isolation.
Intentionally daemonizing Unix children that detach from the group are outside
this trusted-client contract.

Cancellation/timeout kills the process tree and closes the executor's pipes.
A downstream write failure also kills the tree. Natural parent exit terminates
lingering children so inherited pipe handles cannot keep the run hanging.
The direct child and all local worker goroutines are joined before return.
Output is drained through EOF and successful process exit is required; EOF alone
is not a successful dump. **The caller must make a blocking writer cancelable**
(e.g. close its pipeline pipes when the shared context is canceled); an arbitrary
`io.Writer` blocked inside `Write` cannot be forcibly interrupted by this package.

## Failures and pipeline integration

Stderr is drained with an 8 KiB retained limit and never exposed verbatim.
Numeric client errors classify authentication, missing databases, grants,
connection failures, and database disk-full errors. Filesystem/output disk-full
and permission failures have distinct safe messages. Unknown stderr, raw process
errors, SQL, credentials, and downstream error text are not printed or returned.
Use `errors.Is` with the exported `Err*` classes or `context.Canceled` /
`context.DeadlineExceeded`, not error-message comparison.

Run may have emitted partial SQL before failing. The orchestrator must connect
Run to `artifact.Encrypt` through a backpressured, cancelable pipe, wait for both
stages, and discard ciphertext if **either** fails. Close the writer with Run's
error, not an unconditional EOF. Only the encrypted artifact may be spooled for
upload/retry; never create plaintext or unencrypted compressed retry files.
See [ARTIFACT.md](ARTIFACT.md) for encryption/restore finalization requirements.

## Verification

```sh
go test ./internal/dump ./internal/config
go test -race ./internal/dump ./internal/config
```

Tests cover safe arguments, credential escaping/permissions, ambient environment
isolation, bounded output/stderr, output failures, missing executables, timeouts,
cancellation, spawned children, natural-exit cleanup, and throttling cancellation.
Cross-compilation is not a live Windows Job Object/service validation; that still
needs testing on Windows Server. The target-specific process tests are included
for execution there.

Real integration tests run in **isolated** MySQL 8.4 and MariaDB 11.4 containers
with matching clients, never the primary development database. They cover a
scoped account with a special-character password, views/routines/triggers/events,
binary data, both compression/encryption round trips, authentication rejection,
and inaccessible schemas. Fixtures create/drop their own test objects/accounts.

```sh
docker compose -p db-agent-dump-tests -f docker-compose.diagnostics.yml up -d --wait
CGO_ENABLED=0 go test -tags integration -c -o /tmp/opencode/db-agent-dump.test ./internal/dump
docker compose -p db-agent-dump-tests -f docker-compose.diagnostics.yml cp /tmp/opencode/db-agent-dump.test mysql:/tmp/db-agent-dump.test
docker compose -p db-agent-dump-tests -f docker-compose.diagnostics.yml exec -T -e DB_AGENT_DUMP_INTEGRATION=1 mysql /tmp/db-agent-dump.test -test.run '^TestLogicalDumpFixture$' -test.v
docker compose -p db-agent-dump-tests -f docker-compose.diagnostics.yml cp /tmp/opencode/db-agent-dump.test mariadb:/tmp/db-agent-dump.test
docker compose -p db-agent-dump-tests -f docker-compose.diagnostics.yml exec -T -e DB_AGENT_DUMP_INTEGRATION=1 mariadb /tmp/db-agent-dump.test -test.run '^TestLogicalDumpFixture$' -test.v
docker compose -p db-agent-dump-tests -f docker-compose.diagnostics.yml down --volumes
```

No control-plane changes are required for this execution stage.
