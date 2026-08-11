# Registry push throughput — analysis and improvement plan

Status: analysis of the code as of commit `0094e9d`. Steps 1–4 are implemented;
step 5 is deliberately left at its current default, for the reason recorded
under it.

## Observed symptom

On a 1 Gbit/s link the push to a remote registry never exceeds roughly
8–10 MB/s, i.e. under 10% of the available bandwidth. The number is stable,
which points at a structural stall rather than at congestion or packet loss.

## What is *not* the cause

- **Compression.** Data layers are produced by `ociimg.NewFileLayer`
  (`pkg/ociimg/filelayer.go`) and land on disk already compressed;
  `fileLayer.Compressed()` is a plain `os.Open`. The upload path never runs a
  codec, so compression cannot throttle it. The in-memory `ociimg.NewLayer`
  path likewise compresses before the push begins.
- **TLS.** With AES-NI, Go's TLS stack sustains well above 1 GB/s — two orders
  of magnitude above the observed rate.
- **Digest computation.** Layer digests are computed once at build time and
  cached (`pkg/ociimg/layer.go`, `pkg/ociimg/filelayer.go`); the pusher only
  replays bytes.

## Root cause: serial chunked PATCH

`pusher.doUpload` (`pkg/registry/push.go:520`) implements the OCI chunked
upload strictly serially:

```
ReadFull(chunk) → PATCH → wait for the response → ReadFull → PATCH → …
```

Each `PATCH` is a full HTTP request, and most registries **persist the chunk to
their backing store before answering** (on S3-backed registries this is an
S3 multipart part upload). The achievable throughput is therefore

```
chunk_size / (chunk_size / bandwidth + ack_latency)
```

`pkg/backup/pipeline.go:1325` hardcodes `ChunkSize: 8 << 20`, overriding the
32 MiB default declared in `PushOptions` (`pkg/registry/push.go:69`). With
8 MiB on a gigabit link the transfer of one chunk takes ~0.07 s, so an ack
latency of 0.6–0.8 s — entirely ordinary for a remote, object-storage-backed
registry — yields ~10 MB/s. The link is idle for roughly 90% of the wall clock.

With the default `--max-layer-size 1GiB`, that is 128 strictly serialised
round trips per layer.

## Contributing factors

1. **HTTP/2 multiplexing.** Every registry client in the package is built on
   `http.DefaultTransport` (`pkg/registry/push.go:103`, `push.go:161`,
   `blob.go:44`, `verify.go:37`), which sets `ForceAttemptHTTP2: true`.
   Against an HTTPS registry all `--jobs` uploads are then multiplexed onto a
   *single* TCP connection. Registries fronted by nginx or envoy commonly
   advertise small per-stream flow-control windows, so the uploads serialise
   and stall waiting for `WINDOW_UPDATE` frames. This is the well-known
   "docker push is slow over HTTP/2" failure mode. While it is in effect,
   raising `--jobs` buys almost nothing.
2. **Untuned connection pool.** `DefaultTransport` has
   `MaxIdleConnsPerHost = 2` while the default job count is 3: one connection
   is repeatedly closed and re-established, paying a TCP and TLS handshake
   each time.
3. **Small write buffer.** `DefaultTransport` uses a 4 KiB write buffer, which
   means thousands of small `write` syscalls per chunk at gigabit rates.
4. **No overlap between disk and network.** In `doUpload` the disk is idle
   while a `PATCH` is in flight and the link is idle during `ReadFull`. Even
   with an instantaneous ack, the two costs add up instead of overlapping.
5. **Conservative job default.** `--jobs` defaults to 3
   (`internal/cli/backup.go:151`). Concurrency across blobs is the cheapest way
   to hide per-request latency, but only once factor 1 is removed.

## Implementation order

The steps are ordered by ratio of expected gain to regression risk. Each one
is independently useful and independently revertable.

### 1. Dedicated HTTP transport (implemented)

Replace `http.DefaultTransport` in the `registry` package with a transport
tuned for large sequential uploads: HTTP/1.1 forced (so each job owns a real
TCP connection), `MaxIdleConnsPerHost` sized on the job count, and a 256 KiB
write buffer. Proxy handling, dial timeouts and TLS defaults are inherited
from `http.DefaultTransport` by cloning it, so no security-relevant default
changes.

Expected effect: removes the flow-control stall and the handshake churn;
makes `--jobs` actually scale. Risk: low — it is a transport-level change with
no protocol impact, and HTTP/1.1 is what every registry supports natively.

Two details matter for correctness. Clearing `ForceAttemptHTTP2` is not enough:
`http.Transport.Clone` materialises a TLS configuration advertising
`NextProtos: ["h2", "http/1.1"]`, so ALPN would still negotiate h2 against a
server that offers it — on a transport that has no h2 handler registered. The
advertised protocol list is therefore pinned to `http/1.1`. And the transport
is process-wide and shared, exactly like the `http.DefaultTransport` it
replaces, because `RegistrySink.client` builds a fresh `BlobClient` per blob:
a per-client pool would mean a new TCP and TLS handshake for every blob.

See `pkg/registry/httpclient.go` and `pkg/registry/httpclient_test.go`.

### 2. Single-request upload (implemented)

Every blob source in the push path is reopenable and has a known size — a file
(`fileLayer.Compressed` is an `os.Open`) or a byte slice — so the whole blob is
now sent as one streamed `PATCH` followed by the finalising `PUT`:
`PushOptions.ChunkSize` defaults to zero, which means "one request per blob".
The body is streamed, never buffered, and `Request.GetBody` reopens it so the
bearer round tripper can still replay the request after a 401.

The request *sequence* is unchanged — `POST`, `PATCH`, `PUT` — which is exactly
what a blob smaller than the chunk size already produced before, so no registry
sees a shape it did not already handle. A single-request `POST ...?digest=` was
rejected as the mechanism: it is optional in the distribution spec and would
have needed a capability probe.

Effect: three round trips per blob instead of one per chunk — 3 instead of ~130
for a 1 GiB layer. Cost: resumability *inside* one blob is gone, so a failed
layer restarts from zero; the per-blob checkpoint (`markDone`) is unaffected,
and at 1 GiB per layer a full retry costs about ten seconds on gigabit.
Registries that cap the request body answer 413; that answer makes the push
fall back to 32 MiB chunks for the offending blob and latches the fallback for
the rest of the run, so a body limit costs one wasted attempt, not a failure.

See `uploadSingle`/`patchStream` in `pkg/registry/push.go` and
`pkg/registry/upload_test.go`.

### 3. Read/write overlap for the streaming path (implemented)

The remote-server path (`registry.BlobClient`) receives bytes over the control
stream: it cannot seek and does not know the blob size in advance, so it must
stay chunked. It now keeps two chunk buffers instead of one — a full buffer is
handed to a background `PATCH` while the caller keeps filling the other, so the
incoming stream and the request in flight overlap instead of alternating. Only
one `PATCH` runs at a time, since each one returns the location of the next.
The chunk default is 32 MiB on both `NewBlobClient` and `RegistrySink`.

Effect: on this path the upload no longer stalls for a full round trip every
32 MiB. Cost: the working set per concurrent upload is 2×chunk (64 MiB at the
default) instead of one chunk, still independent of the blob size. One
behavioural consequence is worth knowing: a failed `PATCH` now surfaces at the
next flush boundary or at `Commit`, not necessarily from the `Write` that
filled the buffer. The error is never dropped — it is sticky and `Commit`
always reports it.

See `BlobUpload.startFlush`/`waitFlush` in `pkg/registry/blob.go`.

### 4. Expose the chunk size and align the defaults (implemented)

`backimage backup --upload-chunk-size` (default `0`, one request per blob)
replaces the hardcoded `8 << 20` in `pushRegistry`, so the pipeline, the
library default and the remote sink now agree. It exists for the registry that
refuses large bodies without waiting for the 413 fallback, and it makes the
behaviour measurable in the field without a rebuild.

### 5. Revisit the `--jobs` default (not applied, on purpose)

The default stays at 3. Raising it is not free here: `--jobs` also drives the
temp-space preflight, `need = jobs × max-layer-size`
(`pkg/backup/pipeline.go:321`), a requirement documented in `README.md` and
`docs/backup.md`. Moving the default from 3 to 6 would double the free disk a
backup demands before it starts, and would fail preflight on hosts where the
current default fits — a regression paid by every user, in exchange for a gain
that step 2 has already largely collected: with one request per blob there are
three round trips per layer left to hide, not a hundred and thirty.

The knob remains available per run for anyone whose registry rewards more
concurrency. Changing the default is a decision for after the measurements
below, not before.

## How to measure

The client, the network and the registry must be separated before drawing
conclusions from any single number:

1. push to a local `registry:2` container — if it is still ~10 MB/s, the
   bottleneck is entirely client-side;
2. push to the real registry with `--jobs 1` and with `--jobs 6` — if the two
   are identical, HTTP/2 multiplexing (factor 1) is dominant;
3. compare a single large layer against many small ones — a strong dependence
   on layer count confirms the per-request round-trip model above.

`make bench-transport` measures the raw encrypted transport only and never
contacts a registry (see `docs/transport-benchmark.md`); it does **not** cover
this path.
