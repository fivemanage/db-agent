# Backup orchestration (DEV-494)

`internal/backup.Runner` combines local preparation, `dump.Run`, the streaming
artifact codec, and `transfer.Uploader`. `db-agent backup --now`, startup, and
scheduled runs execute it. Remote triggers remain deferred until heartbeat has a
stable request/acknowledgement contract; they must call this same runner.

## Lifecycle and locking

The nonblocking `.run.lock` in `backup-transfer` covers preparation, dumping,
staging, upload, completion, status updates, and producer cleanup. Recovery takes
the same lock. Unix uses `flock`; Windows uses `LockFileEx`. Process exit releases
the OS lock automatically; the file is never deleted to avoid locking different
inodes. The uploader retains its separate single-slot lock as defense in depth.
Different configs in the same directory share this slot; run service and manual
commands with the same installed config and account.

The dump starts lazily at the first codec read, after transfer-slot and disk
headroom checks. A synchronous `io.Pipe` applies backpressure from ciphertext
writes through compression/encryption to the dump. No plaintext/unencrypted
compressed files are created. Writer EOF is emitted only after successful dump
exit and credential cleanup. Either stage's failure closes the pipes, cancels
the producer, and joins it before any report networking.

The data path stages finalized ciphertext before PUT, as specified by
`TRANSFER.md`, rather than uploading a non-replayable live dump. Default spool
cap is 1 GiB and free-space reserve is 256 MiB. Before staging, at least the
reserve plus 128 KiB must be available for records/state; each ciphertext write
rechecks available space. External disk consumers can still race these checks:
this is a bound, not an OS reservation. Partial ciphertext is removed even if
saving a failure report also fails. Complete queued ciphertext is intentionally
retained only under transfer's size, age, and session bounds.

## State and heartbeats

Atomic, private, bounded `run.json` contains the latest run's request/backup IDs,
start time, status, encrypted size/checksum, duration, pending flag, activity,
and classified last error. It contains no local DB settings, key material,
dump output, or presigned URLs. Unsafe or invalid state fails closed.

| Transfer phase | Activity | Latest run |
| --- | --- | --- |
| building | backing_up | backing_up, pending |
| ready / PUT | uploading | uploading, pending |
| successful completion being sent | uploading | success, pending |
| completion acknowledged | idle | success, not pending |
| failed dump/transfer | error | failed, pending until acknowledged |
| successful PUT but report unavailable | error | success, pending |

Service heartbeats read shared state rather than an in-memory startup snapshot,
so manual commands in another process are visible. A saved active phase without
a lock owner is reported as error rather than falsely reporting ongoing work.
Manual runs also maintain an independent cancellable heartbeat worker, join it,
then attempt a terminal heartbeat. Rejected credentials cancel and join work;
transient heartbeat failures do not undo an acknowledged upload.

Transfer progress is persisted before observers run. Completion acknowledgement
is saved in run history before removing pending transfer state. On restart,
`Runner.Resume` sends the same terminal metadata or retries the same finalized
ciphertext and identity; it never starts another dump. A failed run stays error
after its failed completion is acknowledged. A successful replay returns to idle.
`db-agent status` exposes the recorded latest outcome, activity, and next scheduled
time. Live service health and richer local status remain DEV-501.

## Scheduling (DEV-498)

`internal/backup.Scheduler` uses robfig/cron's calendar calculation and samples
the wall clock every second. The schedule is five fields, in local time, default
`0 4 * * *`. Timezone prefixes, descriptors/intervals, seconds, malformed expressions,
and impossible dates are rejected by the shared configuration parser.

One `.schedule.lock` owner per config directory protects independent, private,
atomic `schedule.json` updates. This file contains the expression, observation time,
and next run with an offset; it cannot race with `run.json` writers. Scheduling
records the next occurrence before dispatching through `Runner.Run`. Manual,
scheduled, initial startup, and replay work all retain the `.run.lock` invariant.
Busy ticks are consumed without a queue; queued ciphertext/completion also prevents
a new dump. Cancellation/update handoff joins the active trigger before clearing
the recorded next run and releasing the scheduler lock. Crashes may leave stale
schedule state; recorded status must not imply live service health.

Scheduling starts after the first accepted or transiently unavailable heartbeat,
not a rejected credential. A locally ready agent attempts a startup backup only
when no prior run is recorded. The service rereads config on restart, calculates
only future occurrences, and does not catch up missed runs. Saved heartbeat schedule
overrides replace the live policy on its next clock sample without interrupting
work; each new backup reads the saved policy. Machine timezone changes require
restart. Forward clock jumps consume at most one due occurrence; backward corrections
do not replay a consumed instant. DST follows robfig: missing times are skipped and
repeated local hours may run twice at distinct instants (unless still busy).

## Verification

Tests cover both codecs and ciphertext restoration, shared in-flight/terminal
state, lock contention across processes/crash release, failures in dump,
compression/encryption validation, spool, upload, completion, and state saving,
bounded retained artifacts, recovery identity/metadata, disk-headroom checks,
cancellation, and real dump-child cleanup on downstream failure/timeout.
The dump package separately tests child-tree termination on each supported OS.
Deterministic scheduler tests cover offsets, 23/25-hour DST days, missing/repeated
hours, clock corrections, persisted overrides/local edits, restart/timezone changes,
busy triggers, private state failures, single scheduler ownership, and shutdown joins.

Cross-compilation is not a live Windows Server service/Job Object test. The
control-plane transfer endpoints still need deployment/validation in DEV-514.
