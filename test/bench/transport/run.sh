#!/usr/bin/env bash
# Measure raw TCP and QUIC streams. Requires root only for the netem cells.
set -euo pipefail
cd "$(dirname "$0")/../../.."

if [ "$(id -u)" -ne 0 ] || ! command -v tc >/dev/null 2>&1; then
	echo "bench-transport SKIPPED: root and tc are required for reproducible netem measurements"
	echo "Run: sudo BENCH_BYTES=$((4<<30)) make bench-transport"
	exit 0
fi
for tool in openssl jq awk; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool" >&2; exit 1; }; done

bytes=${BENCH_BYTES:-4294967296}
port=${BENCH_PORT:-7590}
work=$(mktemp -d)
results=test/bench/transport/results.md
server_pid=
cleanup() {
	if [ -n "$server_pid" ]; then kill "$server_pid" >/dev/null 2>&1 || true; wait "$server_pid" >/dev/null 2>&1 || true; fi
	tc qdisc del dev lo root >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
	-keyout "$work/key.pem" -out "$work/cert.pem" -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
pin=$(openssl x509 -in "$work/cert.pem" -outform DER | sha256sum | awk '{print $1}')

cat >"$results" <<EOF
# Raw transport benchmark results

Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)

Each result is the median of three runs. Payload: ${bytes} bytes. The server
only discards bytes; no registry or OCI work is involved.

| RTT | Loss | Transport | MiB/s | Seconds | Client CPU s | Server CPU s | TCP retransmits / QUIC note |
|---:|---:|---|---:|---:|---:|---:|---|
EOF

run_one() {
	transport_name=$1
	server_log=$work/server-${transport_name}.log
	go run ./test/bench/transport --mode server --transport "$transport_name" --address "127.0.0.1:${port}" --cert "$work/cert.pem" --key "$work/key.pem" >"$server_log" 2>&1 &
	server_pid=$!
	for _ in $(seq 1 100); do grep -q '^READY ' "$server_log" 2>/dev/null && break; kill -0 "$server_pid" >/dev/null 2>&1 || { cat "$server_log" >&2; return 1; }; sleep 0.05; done
	client=$(go run ./test/bench/transport --mode client --transport "$transport_name" --address "127.0.0.1:${port}" --pin "$pin" --bytes "$bytes")
	wait "$server_pid"
	server_pid=
	server=$(tail -n 1 "$server_log")
	printf '%s\n%s\n' "$client" "$server"
}

median() { sort -n | awk 'NR==2 {print; exit}'; }
for cell in '0.1ms|0%' '10ms|0%' '10ms|0.1%' '50ms|0.5%' '100ms|1%' '200ms|2%'; do
	IFS='|' read -r delay loss <<<"$cell"
	tc qdisc replace dev lo root netem delay "$delay" loss "$loss"
	for transport_name in tcp quic; do
		clients=()
		servers=()
		for _ in 1 2 3; do
			mapfile -t pair < <(run_one "$transport_name")
			clients+=("${pair[0]}")
			servers+=("${pair[1]}")
		done
		client_mib=$(printf '%s\n' "${clients[@]}" | jq -r '.mib_per_second' | median)
		client_sec=$(printf '%s\n' "${clients[@]}" | jq -r '.seconds' | median)
		client_cpu=$(printf '%s\n' "${clients[@]}" | jq -r '.cpu_seconds' | median)
		server_cpu=$(printf '%s\n' "${servers[@]}" | jq -r '.cpu_seconds' | median)
		note='n/a; quic-go aggregate retransmit counter not exported by the stream API'
		if [ "$transport_name" = tcp ]; then note=$(ss -ti "sport = :${port}" 2>/dev/null | awk '/retrans:/{print $0; exit}' || true); fi
		printf '| %s | %s | %s | %s | %s | %s | %s | %s |\n' "$delay" "$loss" "$transport_name" "$client_mib" "$client_sec" "$client_cpu" "$server_cpu" "${note:-n/a}" >>"$results"
	done
done

tc qdisc del dev lo root >/dev/null 2>&1 || true
echo "benchmark table written to $results"
