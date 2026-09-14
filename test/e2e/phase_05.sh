#!/usr/bin/env bash
# Phase 05 e2e: CLI login, encrypted backup, valid OCI pull, blob reuse,
# interrupted push/checkpoint resume, secret hygiene and token refresh gates,
# plus the two paths through a registry that nothing else exercised: the full
# read-back after a push, and a backup encrypted to an age recipient instead of
# a passphrase.
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

echo "==> --verify-after-push full re-reads every published layer"
# The quick level lives inside the push and every phase gets it by default;
# the full one opens the image again as a reader would and recomputes every
# stored digest, and no script had ever run it. A small tree keeps it cheap.
bin/backimage backup "$tree/sub" --repo "$REPO" --tag verified --no-encrypt \
	--allow-degraded --temp-dir "$work/tmp" --verify-after-push full --json \
	>"$work/verified.json" 2>"$work/verified.log"
grep -q 'verifica completa superata' "$work/verified.log" || {
	echo "the full read-back did not report a verdict"; sed -n '1,40p' "$work/verified.log"; exit 1; }
# It has to have re-read something: a verdict over zero layers would pass
# while proving nothing.
grep -qE 'verifica completa superata: [1-9][0-9]* layer riletti' "$work/verified.log" || {
	echo "the full read-back reported no layer re-read"; sed -n '1,40p' "$work/verified.log"; exit 1; }

echo "==> a backup encrypted to an age recipient is restored with its identity"
go build -o "$work/agekeygen" ./test/e2e/tools/agekeygen
recipient=$("$work/agekeygen" "$work/identity.txt")
bin/backimage backup "$tree/sub" --repo "$REPO" --tag aged --recipient "$recipient" \
	--allow-degraded --temp-dir "$work/tmp" --json >"$work/aged.json" 2>"$work/aged.log"
jq -e '.encrypted == true' "$work/aged.json" >/dev/null || {
	echo "a backup with --recipient is not marked encrypted"; cat "$work/aged.json"; exit 1; }
bin/backimage restore "$REPO:aged" --identity "$work/identity.txt" --extract \
	-C "$work/aged-out" --no-preserve-owner >"$work/aged-restore.log" 2>&1 || {
	echo "restore with --identity failed"; sed -n '1,40p' "$work/aged-restore.log"; exit 1; }
cmp "$tree/sub/small.txt" "$work/aged-out/sub/small.txt"
# The identity is the only thing that opens it: the passphrase path must not.
set +e
BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "$REPO:aged" --extract \
	-C "$work/aged-wrong" --no-preserve-owner >"$work/aged-wrong.log" 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || { echo "a recipient-encrypted backup opened without its identity"; exit 1; }

echo "==> no secrets in output or logs"
if grep -R -F "$secret" "$work" --exclude=passphrase.txt --exclude=auth.json >/dev/null; then
	echo "secret leaked into phase 05 output"
	exit 1
fi
# The age secret key is the other credential this phase handles, and it must
# stay in the one file that holds it.
age_secret=$(grep -m1 '^AGE-SECRET-KEY-' "$work/identity.txt")
if grep -R -F "$age_secret" "$work" --exclude=identity.txt >/dev/null; then
	echo "the age secret key leaked into phase 05 output"
	exit 1
fi

echo "==> token refresh and 50-way coalescing"
go test ./pkg/registry -run 'TestTokenMintAndProactiveRefresh|TestProviderCoalescing|TestBearerAuth401RetryOnce' -count=1 >/dev/null

echo "phase 05 e2e OK"
