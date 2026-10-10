# Ciphertext transfer and completion reports (DEV-495)

`internal/transfer` implements uploads independently of dump execution and backup
scheduling. `internal/backup` connects the dump producer, durable activity state,
manual trigger, and whole-run lock (DEV-494); scheduling remains DEV-498.
The running agent replays
queued transfers/reports after a successful heartbeat, in a separate cancellable
worker; upload sessions don't block liveness polling.

## Data path

`Uploader.Run` owns a closeable plaintext reader and streams it through
`artifact.Encrypt` into a private **ciphertext-only** spool. It never writes SQL or
unencrypted compressed bytes to disk. After the codec finalizes and the file is
synced, upload uses a file-backed streaming PUT with a known Content-Length.
Memory use does not grow with backup size. This intentionally stages before PUT,
rather than teeing a non-replayable live dump into a failing network connection.

Initiation happens before PUT, with the original start time and an optional size
estimate. The estimate remains omitted when unknown; final encrypted size and
SHA-256 come from the codec. The PUT independently counts/hashes transmitted bytes
and cannot report success for an incomplete or modified local artifact. Duration
includes encryption, upload, and any intervening offline time, up to the terminal
outcome; report retries reuse the original duration.

Object-storage requests use HTTPS, `application/octet-stream`, and no agent
credential. Redirects are rejected. Error text contains no presigned URL, network
details, SQL, key material, or provider error body. Only classified backup/upload
problems are sent to the completion endpoint.

## Control-plane contract required by DEV-514

- `POST /api/v1/backups/initiate` uses one durable CSPRNG-generated Idempotency-Key.
  All replays use identical initiation metadata, including the start time/estimate.
- A replay must return the **same backup ID and object**, issuing a fresh upload
  URL when necessary. The client rejects a changed backup ID. No separate renewal
  endpoint is assumed; this behavior must be implemented/tested in DEV-514.
- Interrupted/repeated PUTs replace the same object with identical ciphertext.
  Transient transport failures, HTTP 408/429/5xx, and HTTP 401/403 trigger bounded
  retries. The latter two can indicate expired signatures; renewal is attempted
  without trusting or logging provider response bodies. Locally expired URLs are
  renewed before sending bytes.
- Completion accepts identical terminal metadata idempotently, including after a
  response was lost. Conflicting outcomes must not overwrite a terminal backup.

The existing authenticated client owns API retries (three attempts, 45-second
operation budget). This work does not implement or deploy the control-plane APIs.

## Local bounds and recovery

The service uses `<config directory>/backup-transfer`. An uploader has one slot,
protected by a nonblocking OS file lock and an in-process mutex. A second run is
refused until the retained artifact or unacknowledged report is resolved. This is
deliberate backpressure, not a growing queue that can fill a game server's disk.

Default limits:

- One ciphertext artifact, at most **1 GiB**; writes exceeding the cap abort.
- Check available filesystem space before every ciphertext write, preserving
  **256 MiB**, with an initial headroom check of reserve plus 128 KiB before
  staging. Other processes can still consume that reserve concurrently; these
  checks aren't an OS disk reservation.
- At most **three transfer sessions**, each with at most **three PUT attempts**
  and a **30-minute** total network budget. Counts are saved before networking,
  so restart cannot reset the session limit. Jittered exponential delays and
  storage Retry-After are respected; Retry-After cooldown survives restart/polls.
- A retained artifact older than **24 hours** is abandoned at the next session.
- Default service upload throttling is **4 MiB/s** when `backup.throttle_io` is
  enabled. Reads have at most a 32 KiB burst and waits respond to cancellation.

These are constructor options, not dashboard overrides or new config fields.
Choose spool size/budget deliberately for larger databases; there is no multipart
upload in this implementation.

`pending.json` is bounded to 4 KiB and privately/atomically replaced after sync.
It contains identity, timing, estimate, binding, phase, session count, cooldown,
and classified terminal metadata; it contains no key or presigned URL. Ciphertext
and state use Unix owner-only permissions or the protected Windows
System/Administrators DACL. Unsafe/invalid files fail closed.

Recovery states:

1. **building:** a failed intent is saved before encryption. A crash leaves only
   a failed report; incomplete ciphertext is removed and never uploaded.
2. **ready:** finalized ciphertext and metadata are durable. Restart reopens those
   exact bytes, initiates/renews with the same identity, and resumes bounded PUTs.
3. **report:** the terminal outcome is saved **before** contacting the API or
   removing ciphertext. Failed reports and successful-but-unacknowledged reports
   survive restart. Retry sends exactly the same metadata, without another PUT.

Cancellation closes a blocked source, stops/joins HTTP body reads and throttle
waits, removes partial ciphertext, and preserves a classified failed report.
The dump orchestrator must still join its process workers. A process crash after
PUT but before committing the terminal outcome may repeat that same PUT, never
create another backup identity. The slot is freed only after completion is
acknowledged. Reports have no automatic eviction: one small retained report can
block new backups until connectivity/access is repaired, but cannot grow disk
usage indefinitely. Rejected credentials stop runtime work instead of reenrolling.
