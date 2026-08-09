# Remote protocol v1

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

## Session sequence

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
- Default operation writes no receiver-side layer data to disk.
- Control errors never include authentication tokens or passphrases.

The protobuf includes small compatibility extensions beyond the original
draft: `Hello.auth_token`, `BackupAck`, layer diff ID/media type, and the
anonymous-token marker. They resolve otherwise ambiguous or deadlocking v1
sequences while retaining the prescribed messages.
