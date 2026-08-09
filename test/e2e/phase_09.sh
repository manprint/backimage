#!/usr/bin/env bash
# Phase 09 e2e: real QUIC/TLS remote backup, TCP coexistence and resume.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase 09 e2e SKIPPED: docker not available"
	echo "phase 09 e2e OK"
	exit 0
fi
for tool in curl jq cmp tar openssl awk; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

REG_PORT=${PHASE09_REGISTRY_PORT:-5005}
REMOTE_PORT=${PHASE09_REMOTE_PORT:-7581}
METRICS_PORT=${PHASE09_METRICS_PORT:-7582}
NAME=bi-registry-p09
HOST="localhost:${REG_PORT}"
REPO="${HOST}/e2e/quic"
IMAGE="${REPO}:t1"
work=$(mktemp -d)
server_pid=
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase 09 diagnostics (exit $rc)" >&2
		for log in server.log resumed.err resumed.json cross-tcp.err cross-quic.err acl.err auth.err; do
			if [ -f "$work/$log" ]; then echo "[$log]" >&2; sed -n '1,80p' "$work/$log" >&2; fi
		done
	fi
	if [ -n "$server_pid" ]; then kill "$server_pid" >/dev/null 2>&1 || true; wait "$server_pid" >/dev/null 2>&1 || true; fi
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

mkdir -p "$work/tree/sub" "$work/tmp" "$work/server-work" "$work/out"
printf 'phase 09 QUIC remote\n' >"$work/tree/sub/a.txt"
dd if=/dev/urandom of="$work/tree/random.bin" bs=1M count=32 status=none
printf 'phase09-shared-secret\n' >"$work/token"
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
		--max-sessions 4 --rate-limit 4MiB \
		--metrics-address "127.0.0.1:${METRICS_PORT}" --work-dir "$work/server-work" \
		>"$work/server.out" 2>"$work/server.log" &
	server_pid=$!
	for _ in $(seq 1 100); do
		curl -fsS "http://127.0.0.1:${METRICS_PORT}/healthz" >/dev/null 2>&1 && return
		kill -0 "$server_pid" >/dev/null 2>&1 || { cat "$work/server.log"; return 1; }
		sleep 0.05
	done
	return 1
}

stop_server() {
	if [ -n "$server_pid" ]; then
		kill "$server_pid" >/dev/null 2>&1 || true
		wait "$server_pid" >/dev/null 2>&1 || true
		server_pid=
	fi
}

echo "==> QUIC/TLS and TCP coexistence, digest parity"
start_server --udp --also-tcp
created=2026-08-09T10:00:00Z
bin/backimage backup "$work/tree" --repo "$REPO" --tag t1 \
	--remote "127.0.0.1:${REMOTE_PORT}" --udp --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded \
	--max-layer-size 8MiB --temp-dir "$work/tmp" --created "$created" --json >"$work/remote.json"
bin/backimage backup "$work/tree" --repo "$REPO" --tag tcp \
	--remote "127.0.0.1:${REMOTE_PORT}" --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded \
	--max-layer-size 8MiB --temp-dir "$work/tmp" --created "$created" --json >"$work/tcp.json"
bin/backimage backup "$work/tree" --repo "$REPO" --tag local \
	--no-encrypt --allow-degraded --max-layer-size 8MiB --temp-dir "$work/tmp" \
	--created "$created" --json >"$work/local.json"
[ "$(jq -r .digest "$work/remote.json")" = "$(jq -r .digest "$work/local.json")" ]
[ "$(jq -r .digest "$work/tcp.json")" = "$(jq -r .digest "$work/local.json")" ]

echo "==> runnable image round-trip"
docker pull "$IMAGE" >/dev/null
docker run --rm "$IMAGE" verify --json | jq -e '.ok and .full' >/dev/null
docker run --rm "$IMAGE" tar >"$work/out/backup.tar"
tar -xf "$work/out/backup.tar" -C "$work/out"
cmp "$work/tree/sub/a.txt" "$work/out/tree/sub/a.txt"
cmp "$work/tree/random.bin" "$work/out/tree/random.bin"

echo "==> QUIC fault injection and restart-safe layer resume"
stop_server
start_server --udp --also-tcp
dd if=/dev/urandom of="$work/tree/random.bin" bs=1M count=96 status=none
bin/backimage backup "$work/tree" --repo "$REPO" --tag resumed \
	--remote "127.0.0.1:${REMOTE_PORT}" --udp --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded \
	--max-layer-size 8MiB --temp-dir "$work/tmp" --created 2026-08-09T10:01:00Z \
	--json >"$work/resumed.json" 2>"$work/resumed.err" &
client_pid=$!
for _ in $(seq 1 200); do
	uploaded=$(curl -fsS "http://127.0.0.1:${METRICS_PORT}/metrics" 2>/dev/null | awk '/backimage_bytes_uploaded_total/{print $2; exit}')
	[ "${uploaded:-0}" -ge 8388608 ] && break
	kill -0 "$client_pid" >/dev/null 2>&1 || break
	sleep 0.05
done
stop_server
start_server --udp --also-tcp
wait "$client_pid"
jq -e '.skippedBlobs > 0' "$work/resumed.json" >/dev/null

echo "==> crossed transport hints"
stop_server
start_server --udp
set +e
bin/backimage backup "$work/tree" --repo "$REPO" --tag cross-tcp \
	--remote "127.0.0.1:${REMOTE_PORT}" --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded --json >/dev/null 2>"$work/cross-tcp.err"
cross_tcp_rc=$?
stop_server
start_server
bin/backimage backup "$work/tree" --repo "$REPO" --tag cross-quic \
	--remote "127.0.0.1:${REMOTE_PORT}" --udp --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded --json >/dev/null 2>"$work/cross-quic.err"
cross_quic_rc=$?
set -e
[ "$cross_tcp_rc" -ne 0 ]
[ "$cross_quic_rc" -ne 0 ]
grep -Fq 'retry adding --udp' "$work/cross-tcp.err"
grep -Fq 'retry without --udp' "$work/cross-quic.err"

echo "==> ACL, authentication, TLS downgrade, metrics and diskless invariant"
stop_server
start_server --udp --also-tcp
set +e
bin/backimage backup "$work/tree" --repo "${HOST}/denied/x" --tag t1 \
	--remote "127.0.0.1:${REMOTE_PORT}" --udp --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded --json >/dev/null 2>"$work/acl.err"
acl_rc=$?
bin/backimage backup "$work/tree" --repo "$REPO" --tag noauth \
	--remote "127.0.0.1:${REMOTE_PORT}" --udp --tls-ca "$work/server.crt" \
	--no-encrypt --allow-degraded --json >/dev/null 2>"$work/auth.err"
auth_rc=$?
openssl s_client -connect "127.0.0.1:${REMOTE_PORT}" -tls1_2 </dev/null >/dev/null 2>&1
tls12_rc=$?
bin/backimage listen-remote --bind-address 127.0.0.1:7583 --udp --tls-self-signed >/dev/null 2>"$work/noauth-server.err"
server_noauth_rc=$?
set -e
[ "$acl_rc" -eq 3 ]
[ "$auth_rc" -eq 3 ]
[ "$tls12_rc" -ne 0 ]
[ "$server_noauth_rc" -eq 2 ]
curl -fsS "http://127.0.0.1:${METRICS_PORT}/healthz" | grep -qx ok
curl -fsS "http://127.0.0.1:${METRICS_PORT}/metrics" | grep -q backimage_sessions_total
[ -z "$(find "$work/server-work" -mindepth 1 -print -quit)" ]
! grep -F 'phase09-shared-secret' "$work/server.log"

echo "phase 09 e2e OK"
