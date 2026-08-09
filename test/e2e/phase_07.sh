#!/usr/bin/env bash
# Phase 07 e2e: host-side lazy restore and inspection paths.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase 07 e2e SKIPPED: docker not available"
	echo "phase 07 e2e OK"
	exit 0
fi
for tool in curl jq cmp tar; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

PORT=${PHASE07_PORT:-5003}
NAME=bi-registry-p07
HOST="localhost:${PORT}"
REPO="${HOST}/e2e/restore"
IMAGE="${REPO}:t1"
work=$(mktemp -d)
cleanup() {
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
tree="$work/tree"
mkdir -p "$tree/sub" "$work/tmp" "$work/tars" "$work/full" "$work/partial" "$work/layout-full"
printf 'host restore\n' >"$tree/sub/a.txt"
dd if=/dev/urandom of="$tree/random.bin" bs=1M count=8 status=none
secret=phase07-e2e-secret
printf '%s\n' "$secret" >"$work/pass.txt"
chmod 600 "$work/pass.txt"

docker run -d --name "$NAME" -p "${PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do curl -fsS "http://${HOST}/v2/" >/dev/null && break; sleep 0.05; done

echo "==> build and publish fixture"
make embed >/dev/null
printf 'registry-password\n' | bin/backimage login "$HOST" -u test --password-stdin >/dev/null
bin/backimage backup "$tree" --repo "$REPO" --tag t1 --passphrase-file "$work/pass.txt" \
	--allow-degraded --max-layer-size 8MiB --temp-dir "$work/tmp" --json >/dev/null

echo "==> inspect, ls, find, quick/full verify"
bin/backimage --json inspect "$IMAGE" | jq -e '.manifest.chunking.count > 0' >/dev/null
host_list=$(BACKIMAGE_PASSPHRASE="$secret" bin/backimage ls "$IMAGE")
image_list=$(BACKIMAGE_PASSPHRASE="$secret" docker run --rm -e BACKIMAGE_PASSPHRASE "$IMAGE" list)
[ "$host_list" = "$image_list" ]
BACKIMAGE_PASSPHRASE="$secret" bin/backimage --json find "$IMAGE" '**/a.txt' | jq -e 'length == 1' >/dev/null
bin/backimage --json verify "$IMAGE" --quick | jq -e '.ok and .quick' >/dev/null
BACKIMAGE_PASSPHRASE="$secret" bin/backimage --json verify "$IMAGE" | jq -e '.ok and .full' >/dev/null

echo "==> tar, full extract, selective extract"
bin/backimage restore "$IMAGE" -C "$work/tars" --passphrase-file "$work/pass.txt"
tar -tf "$work/tars/restore_t1.tar" | grep -q 'tree/sub/a.txt'
bin/backimage restore "$IMAGE" --extract -C "$work/full" --passphrase-file "$work/pass.txt" --no-preserve-owner
cmp "$tree/sub/a.txt" "$work/full/tree/sub/a.txt"
cmp "$tree/random.bin" "$work/full/tree/random.bin"
bin/backimage restore "$IMAGE" --extract -C "$work/partial" --passphrase-file "$work/pass.txt" \
	--include '**/a.txt' --no-preserve-owner
cmp "$tree/sub/a.txt" "$work/partial/tree/sub/a.txt"
[ ! -e "$work/partial/tree/random.bin" ]

echo "==> wrong passphrase stops before data cache"
rm -rf "$XDG_CACHE_HOME/backimage/layers"
set +e
BACKIMAGE_PASSPHRASE=wrong bin/backimage restore "$IMAGE" -o "$work/wrong.tar" >/dev/null 2>"$work/wrong.err"
rc=$?
set -e
[ "$rc" -eq 4 ]
[ ! -e "$XDG_CACHE_HOME/backimage/layers" ]
[ ! -e "$work/wrong.tar" ]

echo "==> OCI-layout source"
bin/backimage backup "$tree" --repo example.test/e2e/layout --tag t1 --passphrase-file "$work/pass.txt" \
	--allow-degraded --output oci-layout --output-path "$work/layout" --platform linux/amd64 \
	--max-layer-size 8MiB --temp-dir "$work/tmp" --json >/dev/null
bin/backimage restore example.test/e2e/layout:t1 --oci-layout "$work/layout" --extract \
	-C "$work/layout-full" --passphrase-file "$work/pass.txt" --no-preserve-owner
cmp "$tree/random.bin" "$work/layout-full/tree/random.bin"

echo "==> doctor JSON and privilege exit"
set +e
bin/backimage --json doctor "$tree" >"$work/doctor.json"
doctor_rc=$?
set -e
jq -e 'type == "array" and length > 0' "$work/doctor.json" >/dev/null
[ "$doctor_rc" -eq 0 ] || [ "$doctor_rc" -eq 3 ]

echo "phase 07 e2e OK"
