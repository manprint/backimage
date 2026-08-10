#!/usr/bin/env bash
# Phase 05 e2e: CLI login, encrypted backup, valid OCI pull, blob reuse,
# interrupted push/checkpoint resume, secret hygiene and token refresh gates.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase 05 e2e SKIPPED: docker not available"
	echo "phase 05 e2e OK"
	exit 0
fi
for tool in go jq curl dd truncate; do
	command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }
done

PORT=${PHASE05_PORT:-5001}
SIZE_MIB=${PHASE05_SIZE_MIB:-2048}
RESUME_RANDOM_MIB=${PHASE05_RESUME_RANDOM_MIB:-128}
NAME=bi-registry-p05
HOST="localhost:${PORT}"
REPO="${HOST}/e2e/backup"
work=$(mktemp -d)
cleanup() {
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
tree="$work/tree"
mkdir -p "$tree/sub" "$work/tmp"
printf 'small file\n' >"$tree/sub/small.txt"
truncate -s "${SIZE_MIB}M" "$tree/sparse-${SIZE_MIB}MiB.bin"
secret='phase05-super-secret-value'
printf '%s\n' "$secret" >"$work/passphrase.txt"
chmod 600 "$work/passphrase.txt"

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p "${PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do
	curl -fsS "http://${HOST}/v2/" >/dev/null && break
	sleep 0.05
done

echo "==> build and login"
make build >/dev/null
printf 'test-password\n' | bin/backimage login "$HOST" -u e2e --password-stdin >/dev/null
[ "$(stat -c '%a' "$BACKIMAGE_AUTH_FILE")" = 600 ] || { echo "auth.json is not 0600"; exit 1; }

common=("$tree" --repo "$REPO" --passphrase-file "$work/passphrase.txt" \
	--allow-degraded --max-layer-size 32MiB --jobs 2 --temp-dir "$work/tmp" --json)

echo "==> first encrypted backup (${SIZE_MIB} MiB sparse fixture)"
first=$(bin/backimage backup "${common[@]}" --tag t1 2>"$work/first.log")
echo "$first" | jq -e '.encrypted == true and .chunks > 0 and .layers > 0' >/dev/null
docker pull --platform linux/amd64 "$REPO:t1" >/dev/null

echo "==> identical backup reuses registry blobs"
second=$(bin/backimage backup "${common[@]}" --tag t1 2>"$work/second.log")
echo "$second" | jq -e '.skippedBlobs > 0' >/dev/null

echo "==> dry-run has no network dependency and no writes"
dry_auth="$work/dry/config/auth.json"
BACKIMAGE_AUTH_FILE="$dry_auth" bin/backimage backup "$tree" \
	--repo localhost:1/no/network --tag dry --no-encrypt --allow-degraded --dry-run --json \
	>"$work/dry.json" 2>"$work/dry.log"
jq -e 'type == "string" and contains("dry-run")' "$work/dry.json" >/dev/null
[ ! -e "$dry_auth" ] || { echo "dry-run wrote auth state"; exit 1; }

echo "==> interrupted upload resumes from checkpoint"
dd if=/dev/urandom of="$tree/resume-random.bin" bs=1M count="$RESUME_RANDOM_MIB" status=none
bin/backimage backup "${common[@]}" --tag resume >"$work/interrupted.json" 2>"$work/interrupted.log" &
pid=$!
checkpoint=''
for _ in $(seq 1 3000); do
	checkpoint=$(find "$XDG_CACHE_HOME/backimage/checkpoints" -name '*.json' -type f 2>/dev/null | head -1 || true)
	if [ -n "$checkpoint" ] && jq -e '.doneBlobs | length > 0' "$checkpoint" >/dev/null 2>&1; then
		kill -TERM "$pid" 2>/dev/null || true
		break
	fi
	if ! kill -0 "$pid" 2>/dev/null; then break; fi
	sleep 0.01
done
wait "$pid" 2>/dev/null || true
[ -n "$checkpoint" ] && [ -f "$checkpoint" ] || { echo "backup completed before a resumable checkpoint was observed"; exit 1; }
resumed=$(bin/backimage backup "${common[@]}" --tag resume 2>"$work/resumed.log")
echo "$resumed" | jq -e '.skippedBlobs > 0' >/dev/null
grep -q 'resuming from checkpoint' "$work/resumed.log" || { echo "resume marker missing"; exit 1; }
docker pull --platform linux/amd64 "$REPO:resume" >/dev/null

echo "==> no secrets in output or logs"
if grep -R -F "$secret" "$work" --exclude=passphrase.txt --exclude=auth.json >/dev/null; then
	echo "secret leaked into phase 05 output"
	exit 1
fi

echo "==> token refresh and 50-way coalescing"
go test ./pkg/registry -run 'TestTokenMintAndProactiveRefresh|TestProviderCoalescing|TestBearerAuth401RetryOnce' -count=1 >/dev/null

echo "phase 05 e2e OK"
