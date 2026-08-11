#!/usr/bin/env bash
# Phase 06 e2e: the backup image restores itself on amd64 and arm64.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase 06 e2e SKIPPED: docker not available"
	echo "phase 06 e2e OK"
	exit 0
fi
for tool in curl cmp tar jq; do
	command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }
done

PORT=${PHASE06_PORT:-5002}
SIZE_MIB=${PHASE06_SIZE_MIB:-64}
NAME=bi-registry-p06
HOST="localhost:${PORT}"
REPO="${HOST}/e2e/selfextract"
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
mkdir -p "$tree/sub" "$work/tmp" "$work/tar-restore" "$work/direct" "$work/partial"
printf 'small file\n' >"$tree/sub/small.txt"
dd if=/dev/urandom of="$tree/random.bin" bs=1M count="$SIZE_MIB" status=none
secret='phase06-e2e-passphrase'
printf '%s\n' "$secret" >"$work/passphrase.txt"
chmod 600 "$work/passphrase.txt"

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p "${PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do
	curl -fsS "http://${HOST}/v2/" >/dev/null && break
	sleep 0.05
done

echo "==> build real embedded binaries and backup"
make embed >/dev/null
printf 'registry-password\n' | bin/backimage login "$HOST" -u e2e --password-stdin >/dev/null
bin/backimage backup "$tree" --repo "$REPO" --tag t1 \
	--passphrase-file "$work/passphrase.txt" --allow-degraded \
	--max-layer-size 16MiB --jobs 2 --temp-dir "$work/tmp" --json >"$work/backup.json"
docker pull --platform linux/amd64 "$IMAGE" >/dev/null

echo "==> default info and list"
info_output="$(docker run --rm "$IMAGE")"
grep -q 'backup backimage' <<<"$info_output"
list_output="$(BACKIMAGE_PASSPHRASE="$secret" docker run --rm \
	-e BACKIMAGE_PASSPHRASE "$IMAGE" list)"
grep -q 'tree/sub/small.txt' <<<"$list_output"

echo "==> encrypted metadata: nothing about the content is public"
# Labels are readable from the registry without pulling: they must not describe
# the backup, and `info` without the passphrase must not print the sources.
docker inspect "$IMAGE" | jq -e '.[0].Config.Labels["dev.backimage.sources"] == null' >/dev/null
docker inspect "$IMAGE" | jq -e '.[0].Config.Labels["dev.backimage.files"] == null' >/dev/null
docker inspect "$IMAGE" | jq -e '.[0].Config.Labels["dev.backimage.bytes-raw"] == null' >/dev/null
if grep -q "$tree" <<<"$info_output"; then
	echo "info leaks the source path without the passphrase"
	exit 1
fi
bin/backimage --json inspect "$IMAGE" | jq -e '.manifest.sources == null and .manifest.totals.files == 0' >/dev/null
bin/backimage --json inspect "$IMAGE" | jq -e '.manifest.private.path == "private.json.zst" and .manifest.schemaVersion == 2' >/dev/null
# With the passphrase the same fields come back, from the encrypted blob.
BACKIMAGE_PASSPHRASE="$secret" bin/backimage --json inspect "$IMAGE" \
	| jq -e --arg tree "$tree" '.manifest.sources == [$tree] and .manifest.totals.files > 0' >/dev/null
priv_info="$(BACKIMAGE_PASSPHRASE="$secret" docker run --rm -e BACKIMAGE_PASSPHRASE "$IMAGE" info)"
grep -q "$tree" <<<"$priv_info"

echo "==> canonical tar round-trip"
BACKIMAGE_PASSPHRASE="$secret" docker run --rm \
	-e BACKIMAGE_PASSPHRASE -i "$IMAGE" tar >"$work/out.tar"
tar -xf "$work/out.tar" -C "$work/tar-restore"
cmp "$tree/sub/small.txt" "$work/tar-restore/tree/sub/small.txt"
cmp "$tree/random.bin" "$work/tar-restore/tree/random.bin"

echo "==> direct and selective extraction"
BACKIMAGE_PASSPHRASE="$secret" docker run --rm \
	--user "$(id -u):$(id -g)" -e BACKIMAGE_PASSPHRASE -v "$work/direct:/restore" "$IMAGE" \
	extract --out /restore --no-preserve-owner >/dev/null
cmp "$tree/sub/small.txt" "$work/direct/tree/sub/small.txt"
cmp "$tree/random.bin" "$work/direct/tree/random.bin"
BACKIMAGE_PASSPHRASE="$secret" docker run --rm \
	--user "$(id -u):$(id -g)" -e BACKIMAGE_PASSPHRASE -v "$work/partial:/restore" "$IMAGE" \
	extract --out /restore --include '**/small.txt' --no-preserve-owner >/dev/null
cmp "$tree/sub/small.txt" "$work/partial/tree/sub/small.txt"
[ ! -e "$work/partial/tree/random.bin" ]

echo "==> negative passphrase and partial verification"
set +e
BACKIMAGE_PASSPHRASE=wrong docker run --rm -e BACKIMAGE_PASSPHRASE "$IMAGE" list \
	>"$work/wrong.out" 2>"$work/wrong.err"
rc=$?
set -e
[ "$rc" -eq 4 ]
[ ! -s "$work/wrong.out" ]
docker run --rm "$IMAGE" verify | grep -q 'verifica parziale'
docker run --rm "$IMAGE" verify --json | grep -q '"ok":true'

echo "==> arm64 bootstrap"
arm_info=$(docker run --rm --platform linux/arm64 "$IMAGE" info)
grep -q 'backup backimage' <<<"$arm_info"

echo "phase 06 e2e OK"
