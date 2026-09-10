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

## The same audit on `listen-remote`

The five steps above cover a `backup` that pushes from the client. A backup
that goes through `backimage listen-remote` reaches the registry from the
server, on a path that had not been looked at and did not inherit any of them.
Four findings, all applied.

### 6. Reception and the registry push were strictly serialised

`streamBuilder.run` rolled a layer inline: digest the spool, rebuild it into
its OCI blob, then upload it — all on the goroutine draining the client's
`io.Pipe`. While that ran, nothing read the pipe, the session stopped reading
frames, the receive window closed, and the client stopped sending. Reception
and the slowest stage of the pipeline never overlapped, so the wall clock was
their sum: `t = t_receive + t_push`, when it could be `max(t_receive, t_push)`.

The layer tail now runs on its own goroutine. The handoff is unbuffered, which
keeps exactly one layer in flight — the back-pressure that stops a fast client
from accumulating spools — and raises the `--work-dir` requirement from two to
three times the layer size. `TestReceptionOverlapsTheRegistryPush` pins the
behaviour by making every upload slow and asserting that bytes still arrive
during one.

### 7. The server could never reach the one-request-per-blob path

Step 2 made `ChunkSize: 0` mean "one streamed `PATCH` per blob". The server
never got there: `NewRegistrySink` coerced any non-positive chunk size to
32 MiB, and `listen-remote` exposed no flag to change it. Every server-side
push therefore paid a round trip per 32 MiB — the exact cost step 2 removed
from the client.

`RegistrySinkOptions.ChunkSize` now keeps the caller's zero, and
`--upload-chunk-size` exposes it. The distinction that matters is whether the
source can be rewound:

- a v1 layer arrives as a stream the server cannot seek, so it still goes
  through the 32 MiB double buffer of `BlobClient.Open`;
- a v2 layer is a file in `--work-dir`, so it goes through the new
  `BlobClient.PutStream`: three round trips instead of one per chunk, with the
  same 413 fallback.

### 8. `--push-jobs` was wired to `--max-sessions`

`listen_remote.go` passed `Jobs: maxSessions` into the sink. They are different
quantities: one bounds concurrent clients, the other bounds parallel blob
uploads inside a single push. A server at its default of 4 sessions authorised
16 concurrent uploads. They are now separate flags, `--push-jobs` defaulting
to 3 like the library.

### 9. The client alternated with the wire

`runStream` wrapped the archiver in a plain `bufio.Writer`: every flush stopped
the filesystem walk for the duration of the send. `remote.FrameBuffer` replaces
it with two buffers — the walk fills one while the other is on the wire — which
is the same double-buffering `BlobUpload` already used for the registry side.

### What is still on the table

- **The stored bytes go through the codec twice, and the second pass gains
  nothing.** A chunk is compressed and sealed into the spool (pass 1, the real
  compression), and `ociimg.NewFileLayer` then runs the codec again over the
  tar wrapping that spool (pass 2), because an OCI layer *is* a tar and its
  media type declares the codec: `…layer.v1.tar+zstd`. Pass 2 therefore
  compresses data that is already compressed, and on an encrypted backup is
  AEAD output, i.e. indistinguishable from random.

  Measured on this machine, per 256 MiB at the zstd default level 2:

  | stage | throughput | size change |
  |---|---:|---:|
  | pass 1, zstd over plaintext chunks | data-dependent | the actual saving |
  | pass 2, zstd over the sealed spool | ~1500 MiB/s | −0.002% (it grows) |
  | AES-256-GCM seal | ~2900 MiB/s | — |
  | sha256, one of 3-4 passes | ~2300 MiB/s | — |

  So it is waste, but small waste: zstd detects incompressible blocks and
  stores them, leaving pass 2 at roughly memcpy speed. At ~12 Gbit/s it is not
  the server bottleneck on any link this tool is likely to see, and it is not
  server-specific either — the local pipeline does the same two passes
  (`pkg/backup/pipeline.go:986` and `:1061`).

  Removing it means giving the layer wrapper the `store` codec and a plain
  `…layer.v1.tar` media type. That changes the layer digest, so it breaks
  dedup against every existing backup and the local/remote digest parity. Not
  worth 170 ms per 256 MiB.
- **No buffering between pipeline stages.** Network → chunker and chunker →
  tar scanner are both unbuffered `io.Pipe`s, so the three goroutines hand off
  in lockstep with no slack. The kernel receive buffer absorbs some of it; a
  concurrent read-ahead between the stages would absorb the rest. Deliberately
  not done: it means reimplementing the error propagation the ingest path
  relies on (`CloseWithError` in both directions, `errStreamAborted`, the
  sticky cause surfaced to the client), and the gain only appears once
  compression and the link are comparable — around 10 GbE, not on the gigabit
  links this is used on.
- **None of this has been measured on a real link.** As with steps 1-5, the
  reasoning is backed by tests, not by a throughput number. The overlap test
  proves the property, not the size of the gain.

### 10. The temp-space preflight described a window that does not exist

Not a remote finding, but found while answering "how much local disk does a
remote backup need?" and it changes that answer.

`pipeline.go` required `jobs × max-layer-size` of free space. But `rollLayer`
drops the spool and **keeps** the file `ociimg.NewFileLayer` produced, appending
it to `b.data`; those are only released by `cleanup()`, deferred to the end of
`Run` — that is, after the push. Every layer is on disk at once for the whole
upload.

Measured: a 1 GiB incompressible source with `--jobs 1 --max-layer-size 64MiB`
→ preflight asked for 64 MiB, real `--temp-dir` peak **1024 MiB**. A factor of
16, growing with the layer count. A 20 GiB source on a 5 GiB disk passed the
preflight and then died of ENOSPC mid-run.

The preflight now asks for the stored upper bound (`raw + 2 × layer`) and its
error names the two remedies that work: `--temp-dir`, or `--remote-mode stream`
which builds nothing locally (measured: 4 KiB of client spool for the same
1 GiB).

Making the peak genuinely small is a separate change: push each layer as it is
built and replace it with a descriptor, the way the server already does in
`CommitStream`. It touches the `build → finalize → buildImages → push`
ordering, the checkpoint resume, and the `--output oci-layout/tar/daemon`
paths that need the bytes at write time. Not attempted here.

---

## The restore side

The audit above is about pushing a backup. Reading one back had two costs of
its own, both quadratic in the number of chunks rather than linear in the size
of the data, and both removed in 0.5.0.

### 11. Every chunk re-read its layer blob from the start

The chunks of a data layer are stored concatenated in one file, so reaching
chunk *i* means reaching the sum of the stored sizes before it.
`recovery.StoredChunk` did that with `io.CopyN(io.Discard, r, offsets[i])`:
it re-read and threw away everything before the chunk it wanted.

For a 1 GiB layer of 16 MiB chunks — 64 chunks — a full restore therefore read
about **32 GiB to deliver 1 GiB**, n²/2 instead of n. It hit `LocalSource`,
which is the path used by the self-extracting image and by any backup already
unpacked on disk.

`LocalSource.Open` returns an `*os.File`, so the reader is an `io.Seeker`: the
discard is now a single `lseek`, and the copy stays only as the fallback for a
source that cannot seek. Measured on the unit fixture: 123 789 bytes read to
deliver 10 752 bytes of chunks before, 10 752 + metadata after.

### 12. A layer with no room in the cache was rebuilt once per chunk

`restore.imageSource.materialize` extracts a data layer into a file under the
layer cache. When the cache is disabled (`--cache-size 0`) or the layer does
not fit in it, the file was created as a temporary and `Blob` removed it with
`defer os.Remove(path)` — after reading **one** chunk out of it.

So every chunk paid for the whole layer: download, decompress, write to disk,
read 16 MiB, delete. A 1 GiB layer of 64 chunks meant 64 GiB of I/O and 64
decompressions. Note that an OCI layout never keeps a cache at all
(`FromOCILayout` passes no `CacheSize`, which means "never keep a layer"), so
`--oci-layout` and the self-extracting image took the worst case every time.

The temporary now lives as long as the layer instead of as long as the chunk,
with a small cap of live layers (`ephemeralLayerCap`) so the selective and
partial paths, which jump between entries and can alternate layers, do not go
back to rebuilding on every jump. `Close` removes what is left. Measured by
`TestCacheDisabledPruneAndContext`: one download for the two chunks of a
layer, where the old code did two.

The cache policy itself is unchanged — this decides nothing about *whether* to
keep a layer, only about not rebuilding one that is already there.
