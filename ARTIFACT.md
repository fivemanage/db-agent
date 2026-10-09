# Encrypted backup artifact, version 1 (DEV-493)

`internal/artifact` implements `dump → compress → encrypt` and the reverse
pipeline. It uses ordinary `io.Reader`/`io.Writer` streams, creates no files, and
never needs the entire dump, compressed stream, or ciphertext in memory.
Backup scheduling, dump execution, upload, and the restore CLI remain separate
work. This document fixes the wire format, not the exact compressor output.

## Keys and cryptography

The master key is the **32 raw bytes** in the existing `dbak1_` recovery key (see
`internal/key`), not the ASCII recovery string and not its SHA-256 fingerprint.
The zero-value key is rejected as an uninitialized key. The caller loads and
validates the private key and acknowledgement locally; the artifact package
does not manage files, generate recovery keys, or change key lifecycle settings.
Never send a key, key file/path, or recovery string to the control plane.

Each encryption call generates a fresh **32-byte salt** with Go's cryptographic
RNG. Derive a per-artifact key using HKDF-SHA-256 (RFC 5869):

```text
IKM  = raw 32-byte master key
salt = header bytes [12:44]
info = ASCII "db-agent artifact v1 AES-256-GCM" (no NUL or newline)
L    = 32 bytes
PRK  = HMAC-SHA-256(salt, IKM)
key  = HMAC-SHA-256(PRK, info || 0x01)
```

Use AES-256-GCM with that derived key, a 12-byte nonce, and a full 16-byte tag.
The implementation uses Go's `crypto/hkdf`, `crypto/aes`, and `crypto/cipher`;
it does not implement the primitives itself. **This composition is documented
for independent review; it has not received an external cryptographic audit.**

Salt uniqueness makes the deterministic record nonces safe across artifacts
encrypted with the same master key. Never reuse a salt with that key for another
encryption, even to retry or resume an upload. There is no append/resume encryption
API: retry an existing complete ciphertext unchanged, or regenerate the whole
artifact with a fresh salt and fresh completion metadata.

## Header (44 bytes)

All integer fields use unsigned big-endian encoding. Offsets are zero-based.

| Offset | Length | Value |
| --- | --- | --- |
| 0 | 8 | Magic: hex `44 42 41 47 42 41 4b 00` (`DBAGBAK` followed by NUL) |
| 8 | 1 | Artifact version: `1` |
| 9 | 1 | Compression: `1` = zstd, `2` = gzip |
| 10 | 1 | Encryption suite: `1` = HKDF-SHA-256 + AES-256-GCM |
| 11 | 1 | Reserved: MUST be `0` |
| 12 | 32 | Random public salt |

The header contains only public format metadata. It contains no key fingerprint,
database name, host, credentials, SQL, timestamp, or backup ID. Every record's
authentication covers the exact full header, including the salt. Header values
are untrusted until a record authenticates; unknown versions, suites,
compression IDs, or nonzero reserved fields are rejected without guessing a
fallback. Any future wire-format change needs a new version and decoder.

## Records

Compress the entire dump once, then divide the compressed byte stream into
**65,536-byte chunks**. Each data record contains:

```text
length      4 bytes: compressed plaintext length, 1..65,536
ciphertext  exactly length bytes
tag         16 bytes
```

Every data record is full-sized except an optional final short data record.
A short data record must be followed immediately by the terminal record.
Compression frame/member boundaries have no relationship to record boundaries.
Do not compress each chunk independently.

Records are numbered consecutively from zero. For record number `i`:

```text
nonce = eight zero bytes || uint32_be(i)
AAD   = exact 44-byte header || exact four length bytes
sealed = AES-256-GCM.Seal(key, nonce, compressed_chunk, AAD)
```

`sealed` is ciphertext followed by its tag. The record number is implicit, bound
by its nonce; it is not stored in the artifact. Reordering, removing, duplicating,
or splicing data records fails authentication. Check length bounds before reading
or allocating, authenticate before exposing compressed bytes to the decoder,
and never release plaintext from a failed GCM operation.

After all data records, append a **mandatory terminal record**: four zero length
bytes followed by the GCM tag for an empty plaintext at the next record number.
No ciphertext follows its length. Authenticate this record and require exact
source EOF immediately after its tag. Missing/incomplete terminal records are
truncation errors; trailing bytes or concatenated artifacts are corruption errors.
This prevents accepting an authenticated prefix as a complete backup.

At most `2^32 - 1` data records are permitted, reserving nonce `0xffffffff`
for the terminal record. Maximum compressed data is therefore **256 TiB minus
64 KiB**. Reject overflow before sealing/opening another data record; never wrap
the counter. Actual deployments should apply smaller operational size/time limits.

## Compression and memory limits

- Default: zstd (RFC 8878), using `github.com/klauspost/compress/zstd`, one
  synchronous encoder/decoder, an 8 MiB maximum window, and frame checksums.
  Empty dumps still produce a real empty frame. Independent encoders must not
  advertise a window above 8 MiB. Decoder window/memory limits are explicit;
  the library's large defaults are not used. Standard concatenated/skippable
  zstd frames are handled by the decoder.
- Alternative: gzip (RFC 1952), using Go's standard library, including gzip
  CRC/size validation. Standard concatenated gzip members are decoded.
- A terminal record alone is not a valid compressed backup. Empty dump input
  still has compressed data records followed by a terminal record.
- Framing buffers are fixed at 64 KiB plus a tag; copy buffers are 32 KiB.
  Compressor/decoder working memory is bounded by fixed codec/window settings,
  not dump size. No background pipeline queues or full-stream buffers are used.
  The encoder's working tables can exceed the window size; 8 MiB is **not** a
  promise that total process memory stays below 8 MiB.
- Streaming limits memory, not decompressed output size or CPU time. Restore
  orchestration must apply cancellation, deadlines, and disk/output limits to
  prevent resource exhaustion, even for authentic artifacts.

## API, integrity, and failure handling

```go
metadata, err := artifact.Encrypt(ctx, ciphertextWriter, dumpReader, recoveryKey, artifact.Zstd)
metadata, err = artifact.Decrypt(ctx, plaintextWriter, ciphertextReader, recoveryKey)
```

Empty compression selects zstd. Decrypt selects compression from the authenticated
artifact, not today's config. Neither call closes the supplied streams. The
caller must make blocking I/O cancelable (for example, close its pipe/network
body when the context is canceled). Context checks occur at stream reads/writes.

On success, `Metadata` includes version, compression, encrypted `SizeBytes`,
and `Checksum` in `sha256:<64 lowercase hex digits>` form. Both count/hash the
**entire artifact**: header, record lengths, ciphertext, tags, and terminal
record. Encryption measures bytes actually accepted by the destination; decryption
measures bytes consumed from the source. Metadata is zero on every failure and
must never be reported as a successful backup.

Use `errors.Is` to classify `ErrAuthentication` (wrong key or modified artifact),
`ErrTruncated`, `ErrCorrupt`, `ErrInvalidFormat`, `ErrUnsupportedVersion`,
`ErrUnsupportedFormat`, `ErrTooLarge`, and `ErrInvalidKey`. Wrapped I/O and
context errors retain their identity. Malformed authenticated compressed bytes
are corruption, not evidence of a wrong recovery key. Do not forward raw I/O
errors to the dashboard; use the control-plane client's safe error classification.

**Restore is not all-or-nothing output.** Earlier authenticated data records may
already have produced SQL when a later tag, compression checksum, terminal
record, or source EOF check fails. A restore command must write to a private
staging output, delete it on any failure, and publish it only after decryption
and any expected ciphertext checksum/size comparison succeed. Never pipe this
API directly into a live database. Backup callers likewise discard partial
ciphertext on failure. The library creates no plaintext or compressed temp files.
Go/codec memory is not guaranteed to be securely erased, although owned copy,
record, raw-key, and derived-key buffers are cleared where practical.

GCM authenticates an artifact under a recovery key; it does **not** prove which
backup ID or database it belongs to. A trusted expected checksum/size can bind a
download to its completion record and detect substitution of another valid
artifact encrypted with the same key. SHA-256 alone is not a keyed authenticator.

## Control-plane integration (later, outside this change)

No control-plane change is needed to use this local codec. For upload/restore:

1. Store the returned encrypted size/checksum using the existing successful
   completion contract; never calculate them from plaintext or compression alone.
2. Store/serve ciphertext as opaque bytes through the presigned upload/download
   contracts. Do not transform it or decrypt it on the server; no recovery key is
   sent to the server. The header is sufficient to choose the restore codec;
   extra compression/encryption fields are not required for decoding.
3. Return the trusted completed checksum and encrypted size alongside a download
   URL if restore should verify the requested backup's identity. The current
   agent download response only contains URL/expiry; extend that typed contract
   when implementing DEV-499/DEV-514.
4. Arrange storage transfer/retry separately (DEV-495): an unknown-length stream
   may need multipart upload or a completed **encrypted-only** spool for a PUT
   requiring Content-Length. Reuse complete ciphertext for byte-identical retries;
   never resume encryption with a reused salt/counter.

## Independent test vector

All keys below are public **test-only** material, never production keys.
This vector was generated with Node's `crypto.hkdfSync`, `createCipheriv`, and
`zlib.gzipSync`; tests both decode it and reproduce its exact record encryption.
Other gzip compressors need not produce these exact compressed bytes.

```text
master key (hex):
000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
salt (hex):
a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf
derived key (hex):
bfa8038fa74c439fa6bd347c91dbd413302544834f85cd286ddedf2d15c8c3a1
dump (UTF-8, trailing LF): SELECT 1;
compressed bytes (hex):
1f8b08000000000000030b76f571750e5130b4e602007fa90e500a000000
artifact (hex; concatenate lines):
4442414742414b0001020100a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf
0000001e66187b1597126ae2659e39a5c5ec175379bddbae150231400f9b63d8dc9e4e45419596cacbabd31fac5530a49546
0000000015a71f15d1befdba4f1fa8d46da4aff1
size: 114 bytes
checksum:
sha256:21b84b56ec2b6f07b328326f0881105771b24d07df16289a0b34d5ecb31446f7
```

Checks: `go test ./internal/artifact`, `go test -race ./internal/artifact`,
`go test ./internal/artifact -run '^$' -fuzz FuzzDecrypt -fuzztime 10s`, and
`go test ./internal/artifact -run '^$' -bench BenchmarkEncrypt -benchmem`.
The large streaming test uses a backpressured pipe and digest sinks, never an
in-memory artifact or temp file, for 128 MiB of incompressible input per codec.
