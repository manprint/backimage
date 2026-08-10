# Remote protocol (v1 and v2)

This build speaks protocol version 2 and still accepts version 1. Version is
negotiated in `Hello`/`HelloAck`: the server echoes the version the client
asked for, and `HelloAck.streaming` reports whether it can run the v2 pipeline.
A v2 client talking to a v1-only server receives a usage error naming
`--remote-mode layers`.

The remote protocol is defined by
[`pkg/protocol/backimage.proto`](../pkg/protocol/backimage.proto). Generated Go
sources are committed. `make proto-check` regenerates them and fails on drift.

## Transport and framing

TCP is wrapped in TLS 1.3. Every application frame is:

```text
+--------+----------------+------------------+
| type   | length (BE)    | payload          |
| 1 byte | 4 bytes        | length bytes     |
+--------+----------------+------------------+
```

Types are control (protobuf), data (raw compressed OCI layer bytes), and empty
keepalive. Payloads are limited to 4 MiB. The reader checks the declared length
before growing its reusable buffer. Idle streams time out after 120 seconds;
clients send keepalives every 30 seconds.

## Session sequence — v2 (streaming)

```text
Hello -> HelloAck{streaming:true} -> StreamStart -> StreamAck[+TokenRequest]
  -> Data* (raw tar frames, interleaved with StreamProgress and Token)
  -> StreamEnd -> BackupEnd -> close
```

`StreamStart` carries the whole backup policy: reference, tool version,
creation timestamp, codec and level, target layer size, platforms, runnable
flag, source paths and host info (both empty with `--no-metadata`), CDC
parameters, and the `EncryptionConfig`. Data frames carry the raw tar stream
produced by the client archiver; nothing else crosses the wire.

The server derives the file index from the same bytes it chunks, so tar offsets
and per-file digests match the ones the local pipeline would have written
(locked by a parity test against `archive.Writer`). It then builds the
manifest, chunk table, encrypted index blob, metadata layer and per-platform
images, and pushes them with the self-extract binary embedded in the *server*
build.

`StreamProgress` is throttled to one message every two seconds and reports the
stage plus received/stored/uploaded bytes, layers, skipped layers and chunks.
`StreamEnd.raw_bytes` must match the bytes the server accepted, otherwise the
session fails with an integrity error. `BackupEnd` reports the index digest and
the server-side counters.

Because the client sends no digest in advance, the server cannot pre-check a
layer before assembling it; deduplication happens on the layer it just built,
with the same registry `HEAD` used by v1.

### Encryption in v2

`EncryptionConfig` carries `dek`, `nonce_key`, the nonce mode, the age-wrapped
key files and the key fingerprint. The client wraps the keys locally, so the
passphrase never crosses the wire; the DEK does, because the server performs
the sealing. See [remote.md](remote.md) for the trust-boundary consequences.

## Session sequence — v1 (layers)

```text
Hello -> HelloAck -> BackupStart -> BackupAck
  -> (LayerStart -> LayerAck -> Data* -> LayerEnd -> Progress)*
  -> BackupEnd -> close
```

An unexpected message is a usage error and closes the session. `BackupAck`
may include the initial `TokenRequest`, avoiding a write/write handshake
deadlock on unbuffered transports. Later token refreshes use ordinary `Token`
control messages between data frames.

Layer data never enters protobuf. `LayerStart` carries compressed digest,
uncompressed diff ID, media type, and exact size. The receiver hashes bytes as
it forwards them, checks size and digest, then commits the registry upload.
The diff ID lets the receiver construct OCI configs without downloading the
layer it just uploaded.

## Authentication and delegated registry tokens

The `Hello` contains the optional pre-shared receiver authentication token.
The transport may instead authenticate the client with mTLS. Repository
credentials are not sent in `BackupStart`:

1. The receiver requests a repository/actions scope.
2. The client mints a short-lived registry bearer token.
3. The client sends `Token{value, expires_at, repository, actions}`.
4. At 40% lifetime remaining, the client invalidates its local cached token,
   mints a replacement, and sends it proactively.

The receiver keeps delegated tokens only in memory. Waiting for a missing or
expired token is bounded to 30 seconds. Anonymous registries are represented
explicitly without inventing a credential value.

## Resume and safety properties

- A layer digest already present in the target repository is skipped after a
  registry `HEAD`.
- Receiver restart loses no checkpoint state because completed blobs remain in
  the registry.
- Repository ACL and estimated-size quota checks occur at `BackupStart`, before
  data frames.
- Actual bytes are checked again while receiving, so a dishonest estimate
  cannot bypass the quota.
- A v1 session writes no receiver-side layer data to disk; a v2 session writes
  at most the layer currently being assembled, in `--work-dir`, with mode 0600,
  and removes it on every exit path including cancellation.
- Control errors never include authentication tokens or passphrases.
- A v2 session has no mid-stream checkpoint: a retry re-reads the source. Layers
  already published are still skipped by their registry `HEAD`.

The protobuf includes small compatibility extensions beyond the original
draft: `Hello.auth_token`, `BackupAck`, layer diff ID/media type, and the
anonymous-token marker. They resolve otherwise ambiguous or deadlocking v1
sequences while retaining the prescribed messages.
