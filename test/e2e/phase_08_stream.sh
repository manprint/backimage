#!/usr/bin/env bash
# Phase 08 e2e (streaming, protocol v2): the client only walks the filesystem
# and writes a tar on the wire; archiving, compression, encryption, layer
# assembly and the registry push all happen in the remote server process.
# Verified over TCP/TLS and QUIC/TLS.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase 08 stream e2e SKIPPED: docker not available"
	echo "phase 08 stream e2e OK"
	exit 0
fi
for tool in curl jq cmp tar openssl awk du; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

REG_PORT=${PHASE08S_REGISTRY_PORT:-5014}
REMOTE_PORT=${PHASE08S_REMOTE_PORT:-7588}
METRICS_PORT=${PHASE08S_METRICS_PORT:-7589}
NAME=bi-registry-p08s
HOST="localhost:${REG_PORT}"
REPO="${HOST}/e2e/stream"
PASSPHRASE=phase08-stream-passphrase
work=$(mktemp -d)
server_pid=
watch_pid=

cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase 08 stream diagnostics (exit $rc)" >&2
		for log in server.log client.err quic.err layers.err; do
			if [ -f "$work/$log" ]; then echo "[$log]" >&2; sed -n '1,80p' "$work/$log" >&2; fi
		done
	fi
	[ -n "$watch_pid" ] && { kill "$watch_pid" >/dev/null 2>&1 || true; wait "$watch_pid" >/dev/null 2>&1 || true; }
	[ -n "$server_pid" ] && { kill "$server_pid" >/dev/null 2>&1 || true; wait "$server_pid" >/dev/null 2>&1 || true; }
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

mkdir -p "$work/tree/sub" "$work/tmp" "$work/server-work" "$work/out"
printf 'phase 08 streaming\n' >"$work/tree/sub/a.txt"
dd if=/dev/urandom of="$work/tree/random.bin" bs=1M count=256 status=none
printf 'phase08-stream-secret\n' >"$work/token"
chmod 600 "$work/token"

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
	-keyout "$work/server.key" -out "$work/server.crt" -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1

docker run -d --name "$NAME" -p "${REG_PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do curl -fsS "http://${HOST}/v2/" >/dev/null && break; sleep 0.05; done
make embed >/dev/null

start_server() {
	: >"$work/server.log"
	bin/backimage listen-remote \
		--bind-address "127.0.0.1:${REMOTE_PORT}" "$@" \
		--tls-cert "$work/server.crt" --tls-key "$work/server.key" \
		--auth-token-file "$work/token" --allow-repo "${HOST}/e2e/" \
		--max-sessions 2 --work-dir "$work/server-work" \
		--metrics-address "127.0.0.1:${METRICS_PORT}" \
		>"$work/server.out" 2>"$work/server.log" &
	server_pid=$!
	for _ in $(seq 1 200); do
		curl -fsS "http://127.0.0.1:${METRICS_PORT}/healthz" >/dev/null 2>&1 && return
		kill -0 "$server_pid" >/dev/null 2>&1 || { cat "$work/server.log"; return 1; }
		sleep 0.05
	done
	return 1
}

stop_server() {
	[ -n "$server_pid" ] || return 0
	kill "$server_pid" >/dev/null 2>&1 || true
	wait "$server_pid" >/dev/null 2>&1 || true
	server_pid=
}

# watch_client_spool records the largest size the client spool ever reaches.
watch_client_spool() {
	: >"$work/spool-peak"
	(
		peak=0
		while :; do
			size=$(du -sk "$work/tmp" 2>/dev/null | awk '{print $1}')
			[ -n "${size:-}" ] && [ "$size" -gt "$peak" ] && peak=$size
			echo "$peak" >"$work/spool-peak"
			sleep 0.1
		done
	) &
	watch_pid=$!
}

echo "==> TCP/TLS: encrypted streaming backup, server-side pipeline"
start_server
watch_client_spool
BACKIMAGE_PASSPHRASE= bin/backimage backup "$work/tree" --repo "$REPO" --tag tcp \
	--remote "127.0.0.1:${REMOTE_PORT}" --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --server-side-compress \
	--password "$PASSPHRASE" --allow-degraded \
	--max-layer-size 32MiB --temp-dir "$work/tmp" \
	--json >"$work/tcp.json" 2>"$work/client.err"
kill "$watch_pid" >/dev/null 2>&1 || true
wait "$watch_pid" >/dev/null 2>&1 || true
watch_pid=

jq -e '.digest != "" and .layers >= 4 and .chunks > 0 and .bytesStored > 0' "$work/tcp.json" >/dev/null
jq -e '.bytesRaw >= 268435456' "$work/tcp.json" >/dev/null

echo "==> the client never spooled the backup locally"
peak=$(cat "$work/spool-peak" 2>/dev/null || echo 0)
# 4 MiB of slack for filesystem block accounting; a client-side pipeline would
# leave hundreds of MiB here.
[ "${peak:-0}" -le 4096 ] || { echo "client spool peaked at ${peak} KiB"; exit 1; }
[ -z "$(find "$work/tmp" -mindepth 1 -print -quit)" ]

echo "==> distinct server-side stages reached the client"
grep -q 'server\[' "$work/client.err"
grep -qE 'server\[(receiving|pushing|publishing)\]' "$work/client.err"
grep -q 'streaming mode' "$work/client.err"

echo "==> published image restores byte for byte"
docker pull "${REPO}:tcp" >/dev/null
docker run --rm -e BACKIMAGE_PASSPHRASE="$PASSPHRASE" "${REPO}:tcp" verify --json | jq -e '.ok' >/dev/null
docker run --rm -e BACKIMAGE_PASSPHRASE="$PASSPHRASE" "${REPO}:tcp" tar >"$work/out/backup.tar"
tar -xf "$work/out/backup.tar" -C "$work/out"
cmp "$work/tree/sub/a.txt" "$work/out/tree/sub/a.txt"
cmp "$work/tree/random.bin" "$work/out/tree/random.bin"
rm -rf "$work/out"/*

echo "==> backimage restore of the streamed backup"
bin/backimage restore "${REPO}:tcp" --extract --destination "$work/restored" \
	--no-preserve-owner --password "$PASSPHRASE" >/dev/null 2>"$work/restore.err"
cmp "$work/tree/random.bin" "$work/restored/tree/random.bin"

echo "==> server released every temporary file and logged no secret"
[ -z "$(find "$work/server-work" -mindepth 1 -print -quit)" ]
! grep -F 'phase08-stream-secret' "$work/server.log"
! grep -F "$PASSPHRASE" "$work/server.log"
curl -fsS "http://127.0.0.1:${METRICS_PORT}/metrics" | grep -q backimage_bytes_uploaded_total
stop_server

echo "==> QUIC/TLS: same pipeline over UDP"
start_server --udp
dd if=/dev/urandom of="$work/tree/random.bin" bs=1M count=64 status=none
bin/backimage backup "$work/tree" --repo "$REPO" --tag quic \
	--remote "127.0.0.1:${REMOTE_PORT}" --udp --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded \
	--max-layer-size 32MiB --temp-dir "$work/tmp" \
	--json >"$work/quic.json" 2>"$work/quic.err"
jq -e '.digest != "" and .chunks > 0' "$work/quic.json" >/dev/null
docker pull "${REPO}:quic" >/dev/null
docker run --rm "${REPO}:quic" tar >"$work/out/quic.tar"
tar -xf "$work/out/quic.tar" -C "$work/out"
cmp "$work/tree/random.bin" "$work/out/tree/random.bin"
[ -z "$(find "$work/tmp" -mindepth 1 -print -quit)" ]
[ -z "$(find "$work/server-work" -mindepth 1 -print -quit)" ]

echo "==> transport mismatch and flag coherence are reported"
set +e
bin/backimage backup "$work/tree/sub" --repo "$REPO" --tag mismatch \
	--remote "127.0.0.1:${REMOTE_PORT}" --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded --json \
	>/dev/null 2>"$work/mismatch.err"
mismatch_rc=$?
bin/backimage backup "$work/tree/sub" --repo "$REPO" --tag badflag \
	--remote "127.0.0.1:${REMOTE_PORT}" --remote-mode layers --server-side-compress \
	--no-encrypt --allow-degraded >/dev/null 2>"$work/layers.err"
badflag_rc=$?
set -e
[ "$mismatch_rc" -ne 0 ]
grep -q -- '--udp' "$work/mismatch.err"
[ "$badflag_rc" -eq 2 ]
grep -q 'remote-mode stream' "$work/layers.err"

echo "phase 08 stream e2e OK"
