# Remote backups

Remote mode moves the backup pipeline to a separate `backimage listen-remote`
process. The transport is always TLS 1.3, over TCP by default or QUIC with
`--udp`.

Two protocols exist. `--remote-mode` selects one; the default is `stream`.

| | `stream` (protocol v2, default) | `layers` (protocol v1, legacy) |
|---|---|---|
| Client does | filesystem walk + tar on the wire | archive, chunking, compression, encryption, OCI layers |
| Server does | chunking, compression, encryption, OCI layers, dedup HEAD, push | registry HEAD, blob upload, manifest/index |
| Client disk | independent of backup size (transport buffers only) | one spool per concurrent layer |
| Server disk | one layer at a time in `--work-dir` | none |
| Server sees plaintext | **yes** | no |
| Resume after a connection loss | restarts the stream, keeps published layers | restarts the interrupted layer |
| Local/remote digest parity | no (the server builds the metadata) | yes |

In both modes registry credentials stay on the client: the client obtains
short-lived bearer tokens and sends them to the server over the already
authenticated TLS session when the server asks for them. The server must reach
the registry but never needs `backimage login`.

## Streaming mode (default)

```sh
backimage backup /var/lib/data --repo ghcr.io/my-team/backups --tag daily \
  --remote 10.10.2.20:7575 --tls-pin <hex> \
  --auth-token-file token --passphrase-file backup-passphrase
```

The client writes one tar stream in 1 MiB frames and never materialises the
archive, a chunk table or an OCI layer. A 50 GiB backup therefore runs with
about 1 GiB free on the client: what remains is the transport buffer plus the
archiver window, both bounded and independent of the backup size.

The server assembles one layer at a time in `--work-dir`: the stored blob is
spooled, digested, wrapped in its deterministic OCI layer, checked with a
registry `HEAD` and streamed to the registry. Both temporary files are removed
before the next layer starts, so the server needs roughly twice the layer size
(`--max-layer-size`, 1 GiB by default), not the size of the backup.

`--server-side-compress` is an accepted alias for this mode; it is a no-op
because streaming already compresses and encrypts on the server. Combined with
`--remote-mode layers` it is a usage error instead of a silent lie.

### Measured cost (4 GiB, incompressible, `--max-layer-size 512MiB`)

| | value |
|---|---|
| client spool (`--temp-dir`) peak | 4 KiB, i.e. the empty directory |
| client peak RSS, `--no-encrypt` | ~19 MiB |
| client peak RSS with a passphrase | ~280 MiB, dominated by the one-shot age/scrypt key wrap, not by the stream |
| server spool (`--work-dir`) peak | ~1 GiB = 2 × layer size |
| server spool after the run | empty |

The same invariants are asserted by `test/e2e/phase_08_stream.sh` over TCP and
QUIC. A full 50 GiB campaign has not been run yet; the numbers above are what
the implementation was measured at.

### Where the keys live

This is the security trade-off of streaming mode, and it is deliberate:

- the **passphrase never leaves the client**. The client generates the data
  encryption key, wraps it with age (`keys.pass.age`, `keys.age`) locally and
  sends only the wrapped files plus the raw key material for the session;
- the **server receives the DEK and the plaintext stream**, because it is the
  component that chunks, compresses and seals the data. A remote server is
  therefore inside the trust boundary of the backup content;
- the DEK lives only in the memory of the session and is wiped when the session
  ends; it is never written to `--work-dir` or to logs.

If the receiver must not see plaintext, use `--remote-mode layers`: the client
keeps the pipeline, the server only pushes sealed layers, and the local spool
requirements documented in [backup.md](backup.md) apply again.

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

## Trusted LAN without certificate files

TLS 1.3 is mandatory even on a trusted LAN; there is no plaintext transport.
However, a LAN deployment does not need a persistent CA or certificate/key
files. Start the receiver with an ephemeral self-signed certificate:

```sh
printf '%s\n' 'a-long-random-shared-secret' > token
chmod 600 token
backimage listen-remote --bind-address 0.0.0.0:7575 \
  --tls-self-signed --auth-token-file token \
  --allow-repo ghcr.io/my-team/
```

Copy the printed `TLS fingerprint SHA256:<PIN>` to the client and use:

```sh
backimage login ghcr.io --username USER --password-stdin
backimage backup ./data --repo ghcr.io/my-team/backups --tag daily \
  --remote 10.10.2.20:7575 --tls-pin <PIN> \
  --auth-token-file token --passphrase-file backup-passphrase
```

`--tls-cert` and `--tls-key` are not needed in this mode. The self-signed
private key exists only in the receiver process, the certificate expires after
24 hours and a restart produces a new PIN. The client must use `--tls-pin` (or
`--tls-ca` with a CA-signed certificate); a self-signed certificate without a
pin or trusted CA is rejected.

For a completely isolated LAN, `--insecure-no-auth` can replace the shared
token, but it disables client authentication, not TLS. Every host able to
reach the port can then attempt a backup to the allowed repositories, so this
mode requires network isolation and firewalling.

With persistent certificates, use `--tls-cert SERVER.crt --tls-key SERVER.key`
on the receiver and `--tls-ca CA.crt` on the client when the CA is not in the
system trust store. mTLS additionally uses `--tls-ca CA.crt` on the receiver
and `--tls-cert CLIENT.crt --tls-key CLIENT.key` on the client; in that mode a
shared auth token is optional.

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
- `--work-dir` holds the per-layer spool of streaming sessions (default
  `$TMPDIR`). Size it for `2 × --max-layer-size × --max-sessions`. Files are
  created with mode 0600 and removed as soon as the layer is published, on the
  error and cancellation paths too. `layers` sessions stay diskless.
- `--spool` is deprecated: streaming always spools one layer at a time and the
  flag only prints a warning.

The largest registry upload buffer is 32 MiB per session; control/data frames
are capped at 4 MiB. Oversized frame lengths are rejected before allocation.

## Progress

A streaming session reports its stages back to the client, which prints them
prefixed with `server[...]`:

```
server[receiving]: ricevuti 193.0 MiB, archiviati 193.0 MiB, caricati 193.0 MiB, layer 6 (0 saltati), chunk 193
server[pushing]:   ...
server[publishing]: ...
```

`receiving` covers reception, chunking, compression and sealing (they run in
the same pass), `pushing` is a blob upload in flight and `publishing` is the
manifest/index publication. The same counters are exported by
`--metrics-address`.

## Resume and failure behavior

The client retries transport failures five times with 1/2/4/8/16 second
backoff. Errors that cannot improve on retry — authentication, ACL, quota, a
server that does not support streaming — fail immediately.

- **`layers`**: every `LayerStart` triggers a registry `HEAD`, so already
  committed content-addressed layers are answered with `LayerAck{skipped:true}`
  and never retransmitted, including after a receiver restart. A connection
  loss inside a layer restarts that layer.
- **`stream`**: a retry re-reads the source and re-sends the archive from the
  beginning; the interrupted layer upload is aborted on the registry. Layers
  already published are still skipped by their `HEAD` check when the new run
  produces the same content (deterministic boundaries, or `--dedup`), so a
  retry is not necessarily a full re-upload — but it is a full re-read of the
  source. There is no mid-stream checkpoint: the raw stream has no restartable
  offset on the client.

Streaming does not reproduce the local index digest: the server builds the
manifest, chunk table and file index from the bytes it receives, so
`--remote-mode stream` has no local/remote digest parity check. Use
`--remote-mode layers` when that parity is a requirement.

## Metrics

The receiver exports active/total sessions, bytes received/uploaded, skipped
layers, session duration, and errors partitioned by kind. Example:

```sh
curl -fsS http://127.0.0.1:7576/healthz
curl -fsS http://127.0.0.1:7576/metrics
```
