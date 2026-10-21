# Remote protocol (v1 and v2)

This build speaks protocol version 2 and still accepts version 1. Version is
negotiated in `Hello`/`HelloAck`: the server echoes the version the client
asked for, and `HelloAck.streaming` reports whether it can run the v2 pipeline.
A v2 client talking to a v1-only server receives a usage error naming
`--remote-mode layers`. A server may only echo or lower the version: a
`HelloAck` naming a version above the client's is refused as invalid. A
message the session does not expect is an error on both sides, never skipped:
a client that dropped one would wait for a `BackupEnd` the peer is not going to
send.

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
before growing its reusable buffer, and compares it as the 32-bit value it is
before converting it: on a 32-bit build a length above the `int` range would
otherwise turn negative and panic the reader before authentication.

Streams time out after 120 seconds of being **idle**: the deadline moves
forward with every byte transferred, so a long transfer never expires while it
is making progress. Keeping the wire from falling silent is therefore part of
the protocol, not an optimisation — clients send keepalives every 30 seconds
and a streaming server reports progress on a timer (see below). QUIC carries
the same idle timeout with a 15-second keepalive of its own.

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

`StreamProgress` is sent on a two-second timer — not once per received frame —
and reports the stage plus received/stored/uploaded bytes, layers, skipped
layers and chunks. The timer is what keeps the session alive across a slow
pipeline stage: driven by arriving frames, the server would go silent exactly
while a layer upload held the receive path, and the client would drop a
connection that was in fact progressing.
`StreamEnd.raw_bytes` must match the bytes the server accepted, otherwise the
session fails with an integrity error. `BackupEnd` reports the index digest and
the server-side counters.

Because the client sends no digest in advance, the server cannot pre-check a
layer before assembling it; deduplication happens on the layer it just built,
with the same registry `HEAD` used by v1.

A finished layer is digested, rebuilt and pushed on a separate goroutine, so
data frames keep being consumed while the previous layer is on its way to the
registry. The handoff is unbuffered: exactly one layer is in flight, and a
client that outruns the registry is throttled by the receive window rather than
by the server accumulating spools.

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

The client never fills `BackupStart.selfextract_amd64` and `selfextract_arm64`:
each extractor is larger than `MaxFrameSize` (4 MiB). The receiver therefore
builds layer 0 (the self-extract binary) from the extractor embedded in its own
build. When the client detects a published index digest that differs, it reads
the index back from the registry by digest (using its own credentials) and
checks whether all layers except layer 0 — metadata layer 1 and all data layers —
match the client's in digest, size and media type, and whether the platform set
is identical. If so, the client republishes its own image: only its layer 0,
configs and platform manifests travel (about 5 MB per platform), because data
and metadata layers are already on the registry. The tag moves to the client's
index digest and the backup exits 0. If any other layer differs, a platform is
missing or extra, the layer count differs, or the published index cannot be read
or is invalid, the mismatch remains an integrity error (exit 5, no republish)
— this can mean the receiver published unexpected content or a network error
occurred. A failed republish also exits 5 and names the tag still holding the
receiver's extractor in the error message. The repair is the client's: a
client up to 0.6.1 still exits 5 against a receiver of another build. Same
client and server version is
recommended (avoids the extra upload and a window where the tag holds an
unverified image), but no longer required. Security note: between the receiver's
original publish and a successful republish, the tag points to the receiver's
self-extract binary; a malicious v1 receiver holds a delegated push token and
could re-tag after the session until the token expires. Before this fix a
substituted extractor remained on the registry; now it is overwritten.

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

A token only reaches the receiver when the session reads it, and the stream is
ordered: a `Token` sent behind data frames is read after them. Two consequences:

- **Publication.** After `StreamEnd` (v2) or the last `LayerEnd` (v1) the
  receiver publishes — last upload, image, index, read-back — while it keeps
  reading the client, so the refresh the client sends in that window is
  delivered to the publication waiting for it. The client is waiting for
  `BackupEnd` there: keepalives and tokens are accepted, a `Cancel` stops the
  publication, anything else ends the session with a usage error. A client may
  stay silent through a long publication: the session's own deadline expiring
  with nothing read only re-arms the wait. Any other read error, a kernel
  `ETIMEDOUT` on a dead peer included, ends the session.
- **Data phase (residual).** While receiving, a token behind data frames is
  read as soon as the pipeline accepts the frames in front of it. A registry
  request that needs a fresh token while the receive path is held — a chunk of
  the v1 double buffer (32 MiB) or of `--upload-chunk-size`, after the token in
  use expired — waits for a token that is queued behind data. The client
  refreshes when 40% of the lifetime is left, so this needs one such step to
  outlast 40% of the registry's token lifetime (two minutes for a five-minute
  token); the step then fails after the 30-second budget and the client
  retries the session. The default v2 upload, one request per layer, does not
  hold the receive path on a token.

## Resume and safety properties

- A layer digest already present in the target repository is skipped after a
  registry `HEAD`.
- Receiver restart loses no checkpoint state because completed blobs remain in
  the registry.
- Repository ACL and estimated-size quota checks occur at `BackupStart`, before
  data frames.
- Actual bytes are checked again while receiving, so a dishonest estimate
  cannot bypass the quota. The client applies the same rule before sending a
  layer: only layers the server does not skip count towards the announced
  quota.
- A v2 server bounds the received archive while scanning it (entry count, name
  and owner lengths), not after holding all of it in memory.
- A commit the registry refuses aborts the upload session and fails the
  backup; a read-back that disagrees with what was pushed is an integrity
  error (`ErrorIntegrity`, exit 5 on the client).
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
