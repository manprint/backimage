# Remote backups

Remote mode keeps registry credentials on the backup client while moving the
registry upload to a separate `backimage listen-remote` process. The transport
is always TLS 1.3, over TCP by default or QUIC with `--udp`. Layer data is
compressed and encrypted by the client before it crosses the remote connection.

## Quick start with certificate pinning

On the receiver:

```sh
printf '%s\n' 'a-long-random-shared-secret' > token
chmod 600 token
backimage listen-remote \
  --bind-address 0.0.0.0:7575 \
  --tls-self-signed \
  --auth-token-file token \
  --allow-repo ghcr.io/my-team/
```

The server prints `TLS fingerprint SHA256:<hex>`. Copy the fingerprint to the
client:

```sh
backimage backup ./data --repo ghcr.io/my-team/backups --tag daily \
  --remote 10.10.2.20:7575 \
  --tls-pin <hex> \
  --auth-token-file token \
  --passphrase-file backup-passphrase
```

The shared authentication token and the backup passphrase are different
secrets. Neither is logged by the receiver.

## QUIC / UDP

Use `--udp` on both peers to run the same protocol over QUIC. QUIC uses ALPN
`backimage/1`, TLS 1.3 certificate pinning or CA validation, a 10-second
handshake deadline, and one bidirectional stream per backup session.

```sh
backimage listen-remote --udp --also-tcp \
  --bind-address 0.0.0.0:7575 --tls-self-signed \
  --auth-token-file token --allow-repo ghcr.io/my-team/

backimage backup ./data --repo ghcr.io/my-team/backups --tag daily \
  --remote 10.10.2.20:7575 --udp --tls-pin <hex> \
  --auth-token-file token --passphrase-file backup-passphrase
```

`listen-remote --udp --also-tcp` binds QUIC/UDP and TCP to the same numeric
port, so old TCP clients and new QUIC clients can coexist. A crossed client and
server transport fails with a retry hint. UDP can be blocked by enterprise
networks; retry without `--udp` when that happens.

The hidden `--x-quic-*` flags are benchmark-only controls. Protocol v1 is
sequential, so `--x-quic-streams` must remain `1`; current quic-go releases do
not expose a selectable congestion controller. See
[transport benchmark](transport-benchmark.md) for the supported experiments and
the reproducible measurement procedure.

## Authentication modes

The server refuses to start unless at least one client authentication mode is
configured:

1. A pre-shared token with `--auth-token-file` (preferred over putting the
   token on the command line). The comparison is constant-time.
2. mTLS with `--tls-ca`; the client supplies `--tls-cert` and `--tls-key`.
3. `--insecure-no-auth`, which is intended only for an already isolated test
   network and emits a prominent warning.

For a PKI-issued server certificate, configure `--tls-cert` and `--tls-key` on
the server and either the system trust store or `--tls-ca` on the client. A
self-signed certificate must be authenticated with `--tls-pin` or a trusted CA
file. TLS 1.2 and older are rejected.

## Policy and resource limits

- `--allow-repo` is repeatable. A repository outside every allowed prefix is
  rejected before a layer byte is received.
- `--max-sessions` bounds concurrent sessions and therefore total buffering.
- `--max-bytes` sets the per-session byte quota.
- `--rate-limit` throttles received bytes per second per session.
- `--metrics-address` exposes `/healthz` and Prometheus text metrics.
- The receiver is diskless by default. `--work-dir` is not touched. The
  reserved `--spool` fallback is rejected by protocol v1 rather than silently
  writing data.

The largest registry upload buffer is 32 MiB per session; control/data frames
are capped at 4 MiB. Oversized frame lengths are rejected before allocation.

## Resume and failure behavior

The client uses a deterministic session ID and retries transport connection
failures five times with 1/2/4/8/16 second backoff. Every `LayerStart` causes a
registry `HEAD`: already committed content-addressed layers receive
`LayerAck{skipped:true}` and are not retransmitted. This remains true after a
receiver restart because the registry, not receiver memory, is the checkpoint.
A connection loss inside a layer restarts that layer.

The phase-08 implementation still constructs deterministic OCI layers through
the shared local pipeline before transmission. This preserves exact local vs
remote digest parity and restartable readers; it also means the client spool
requirements documented in [backup.md](backup.md) still apply. Protocol v1
does not claim the planned no-full-layer-spool optimization.

## Metrics

The receiver exports active/total sessions, bytes received/uploaded, skipped
layers, session duration, and errors partitioned by kind. Example:

```sh
curl -fsS http://127.0.0.1:7576/healthz
curl -fsS http://127.0.0.1:7576/metrics
```
