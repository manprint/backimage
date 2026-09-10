#!/usr/bin/env bash
# Phase A4 e2e: the remote server does not choose what the client asks its
# credential provider for, and a credential that is not a limited delegation
# does not leave the machine by default.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase A4 e2e SKIPPED: docker not available"
	echo "phase A4 e2e OK"
	exit 0
fi
for tool in curl jq openssl; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

REG_PORT=${PHASEA4_REGISTRY_PORT:-5041}
REMOTE_PORT=${PHASEA4_REMOTE_PORT:-7591}
METRICS_PORT=${PHASEA4_METRICS_PORT:-7592}
GREEDY_PORT=${PHASEA4_GREEDY_PORT:-7593}
NAME=bi-registry-pA4
HOST="localhost:${REG_PORT}"
REPO="${HOST}/e2e/a4"
work=$(mktemp -d)
server_pid=
greedy_pid=
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase A4 diagnostics (exit $rc)" >&2
		for log in server.log greedy.log refused.err forwarded.err scope.err; do
			if [ -f "$work/$log" ]; then echo "[$log]" >&2; sed -n '1,60p' "$work/$log" >&2; fi
		done
	fi
	for pid in "$server_pid" "$greedy_pid"; do
		if [ -n "$pid" ]; then kill "$pid" >/dev/null 2>&1 || true; wait "$pid" >/dev/null 2>&1 || true; fi
	done
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
mkdir -p "$work/tree" "$work/tmp" "$work/server-work"
printf 'phase A4\n' >"$work/tree/a.txt"
printf 'phaseA4-shared-secret\n' >"$work/token"
chmod 600 "$work/token"

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
	-keyout "$work/server.key" -out "$work/server.crt" -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1

docker run -d --name "$NAME" -p "${REG_PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do curl -fsS "http://${HOST}/v2/" >/dev/null && break; sleep 0.05; done
make embed >/dev/null

echo "==> a server that asks for a scope the backup does not need gets nothing"
go build -o "$work/greedyremote" ./test/e2e/tools/greedyremote
"$work/greedyremote" --bind "127.0.0.1:${GREEDY_PORT}" \
	--tls-cert "$work/server.crt" --tls-key "$work/server.key" \
	--auth-token-file "$work/token" --repository "e2e/unrelated" --actions "pull,push,delete" \
	>"$work/greedy.out" 2>"$work/greedy.log" &
greedy_pid=$!
for _ in $(seq 1 200); do grep -q "listening on" "$work/greedy.log" && break; sleep 0.05; done
grep -q "listening on" "$work/greedy.log" || { echo "FAIL: the greedy server did not start"; cat "$work/greedy.log"; exit 1; }

set +e
bin/backimage backup "$work/tree" --repo "$REPO" --tag greedy \
	--remote "127.0.0.1:${GREEDY_PORT}" --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded \
	--temp-dir "$work/tmp" >"$work/scope.out" 2>"$work/scope.err"
rc=$?
set -e
[ "$rc" -eq 3 ] || { echo "FAIL: a foreign scope exited $rc, want 3"; cat "$work/scope.err"; exit 1; }
grep -q "this backup pushes to" "$work/scope.err" || {
	echo "FAIL: the refusal did not name the repository the backup actually targets"; cat "$work/scope.err"; exit 1; }
kill "$greedy_pid" >/dev/null 2>&1 || true
wait "$greedy_pid" >/dev/null 2>&1 || true
greedy_pid=

echo "==> the honest server is served normally"
bin/backimage listen-remote \
	--bind-address "127.0.0.1:${REMOTE_PORT}" \
	--tls-cert "$work/server.crt" --tls-key "$work/server.key" \
	--auth-token-file "$work/token" --allow-repo "${HOST}/e2e/" \
	--max-sessions 4 --metrics-address "127.0.0.1:${METRICS_PORT}" \
	--work-dir "$work/server-work" >"$work/server.out" 2>"$work/server.log" &
server_pid=$!
for _ in $(seq 1 200); do
	curl -fsS "http://127.0.0.1:${METRICS_PORT}/healthz" >/dev/null 2>&1 && break
	sleep 0.05
done
curl -fsS "http://127.0.0.1:${METRICS_PORT}/healthz" >/dev/null

bin/backimage backup "$work/tree" --repo "$REPO" --tag plain \
	--remote "127.0.0.1:${REMOTE_PORT}" --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded \
	--temp-dir "$work/tmp" --json >"$work/plain.json"
jq -e '.digest | startswith("sha256:")' "$work/plain.json" >/dev/null

echo "==> a static registry bearer is not forwarded by default"
# `login --token` is the docker-config case: a credential for the whole
# account, with no expiry and no repository scope. It used to travel to the
# remote server labelled as a delegation, with an invented 24 hours on it.
bin/backimage login "$HOST" --token "not-a-real-pat" >/dev/null
set +e
bin/backimage backup "$work/tree" --repo "$REPO" --tag static \
	--remote "127.0.0.1:${REMOTE_PORT}" --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --no-encrypt --allow-degraded \
	--temp-dir "$work/tmp" >"$work/refused.out" 2>"$work/refused.err"
rc=$?
set -e
[ "$rc" -eq 3 ] || { echo "FAIL: a static credential exited $rc, want 3"; cat "$work/refused.err"; exit 1; }
grep -q "static bearer token" "$work/refused.err" || {
	echo "FAIL: the refusal did not say what the credential is"; cat "$work/refused.err"; exit 1; }
grep -q -- "--forward-static-token" "$work/refused.err" || {
	echo "FAIL: the refusal did not name the way out"; cat "$work/refused.err"; exit 1; }
curl -fsS "http://${HOST}/v2/e2e/a4/tags/list" | jq -e '[.tags[]] | index("static") == null' >/dev/null

echo "==> with the opt-in it travels, and the command says so"
bin/backimage backup "$work/tree" --repo "$REPO" --tag static \
	--remote "127.0.0.1:${REMOTE_PORT}" --tls-ca "$work/server.crt" \
	--auth-token-file "$work/token" --forward-static-token --no-encrypt --allow-degraded \
	--temp-dir "$work/tmp" --json >"$work/forwarded.json" 2>"$work/forwarded.err"
jq -e '.digest | startswith("sha256:")' "$work/forwarded.json" >/dev/null
grep -q "receives a full account credential" "$work/forwarded.err" || {
	echo "FAIL: the opt-in did not declare the trust change in the command output"; cat "$work/forwarded.err"; exit 1; }
curl -fsS "http://${HOST}/v2/e2e/a4/tags/list" | jq -e '[.tags[]] | index("static") != null' >/dev/null

echo "phase A4 e2e OK"
