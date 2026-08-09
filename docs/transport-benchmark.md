# TCP and QUIC transport benchmark

`make bench-transport` measures the encrypted raw transport only: its server
discards the received stream and never contacts an OCI registry. The full run
uses a 4 GiB payload, three repetitions per cell and writes medians to
`test/bench/transport/results.md`.

```sh
sudo BENCH_BYTES=$((4 << 30)) make bench-transport
```

The command checks for root and `tc`; if either is missing it prints an explicit
skip and produces no pretend measurements. It temporarily applies `netem` to
`lo` and removes it on exit. Do not run it on a host whose loopback qdisc is
managed by another service.

| RTT | Loss | Transports | Repetitions | Measurements |
|---:|---:|---|---:|---|
| 0.1 ms | 0% | TCP, QUIC | 3 | MiB/s, total time, client/server CPU |
| 10 ms | 0% | TCP, QUIC | 3 | MiB/s, total time, client/server CPU |
| 10 ms | 0.1% | TCP, QUIC | 3 | MiB/s, total time, client/server CPU |
| 50 ms | 0.5% | TCP, QUIC | 3 | MiB/s, total time, client/server CPU |
| 100 ms | 1% | TCP, QUIC | 3 | MiB/s, total time, client/server CPU |
| 200 ms | 2% | TCP, QUIC | 3 | MiB/s, total time, client/server CPU |

TCP retransmission text is collected with `ss -ti`. The stream-level quic-go
API used by backimage does not expose an aggregate retransmission counter, so
that column is explicitly recorded as unavailable rather than estimated.

## Current status

A 1 MiB QUIC smoke run on loopback completed at 375.6 MiB/s on the client and
418.5 MiB/s on the discard server. This validates the harness and orderly QUIC
shutdown only; it is **not** a 4 GiB, three-run, netem result and must not be
used to select a production transport.

Run the full matrix before adopting a numeric policy. The intended decision
threshold is to use `--udp` only when its median throughput is at least 10%
higher than TCP for the target network while its median completion time under
loss is no worse than TCP; otherwise retain the TCP default. That threshold is
an operational policy awaiting the required measured table, not a claim about
current results.

## Experimental flags

The following hidden options exist only for a controlled benchmark run:

- `--x-quic-window N` changes the initial QUIC stream receive window.
- `--x-quic-gso=false` sets `QUIC_GO_DISABLE_GSO=true` before opening QUIC.
- `--x-quic-streams` is intentionally fixed to `1` by protocol v1.
- `--x-quic-cc` accepts only `cubic`: quic-go v0.61.0 exposes no alternative
  congestion-controller selector.

Normal remote backups need none of these options.
