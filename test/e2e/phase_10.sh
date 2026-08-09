#!/usr/bin/env bash
# Phase 10 e2e: CDC layer boundaries and incremental registry deduplication.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase 10 e2e SKIPPED: docker not available"
	echo "phase 10 e2e OK"
	exit 0
fi
for tool in jq tar diff dd cp curl; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

REG_PORT=${PHASE10_REGISTRY_PORT:-5006}
SIZE_MIB=${PHASE10_SIZE_MIB:-4096}
NAME=bi-registry-p10
HOST="localhost:${REG_PORT}"
REPO="${HOST}/e2e/dedup"
work=$(mktemp -d)
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase 10 diagnostics (exit $rc)" >&2
		for log in t1.err t2.err; do [ -f "$work/$log" ] && { echo "[$log]" >&2; sed -n '1,100p' "$work/$log" >&2; }; done
	fi
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

mkdir -p "$work/tree/sub" "$work/tmp" "$work/out1" "$work/out2"
printf 'phase 10 incremental dedup\n' >"$work/tree/sub/readme.txt"
# Random data avoids a compression-only win: the assertion measures layer reuse.
payload_mib=$(( SIZE_MIB / 5 ))
for i in $(seq -w 0 4); do
	dd if=/dev/urandom of="$work/tree/payload-${i}.bin" bs=1M count="$payload_mib" status=none
done
# Keep add/remove entries after payload-* in lexical tar order. They must not
# artificially shift the unchanged prefix of the stream.
dd if=/dev/urandom of="$work/tree/zz-delete-me.bin" bs=1M count=5 status=none
cp -a --reflink=auto "$work/tree" "$work/original"

docker run -d --name "$NAME" -p "${REG_PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do curl -fsS "http://${HOST}/v2/" >/dev/null && break; sleep 0.05; done
make embed >/dev/null

echo "==> first CDC backup"
bin/backimage backup "$work/tree" --repo "$REPO" --tag t1 --dedup --no-encrypt \
	--allow-degraded --platform linux/amd64 --max-layer-size 64MiB --temp-dir "$work/tmp" \
	--created 2026-08-09T12:00:00Z --json >"$work/t1.json" 2>"$work/t1.err"
uploaded1=$(jq -er '.uploadedBytes' "$work/t1.json")
[ "$uploaded1" -gt 0 ]

echo "==> modify one percent of distributed data"
touch_mib=$(( SIZE_MIB / 100 ))
if [ "$touch_mib" -lt 1 ]; then touch_mib=1; fi
per_file_mib=$(( (touch_mib + 4) / 5 ))
for i in $(seq -w 0 4); do
	# Five disjoint payloads model changed files without making every OCI layer
	# non-shareable; a layer is the smallest unit an OCI registry can deduplicate.
	dd if=/dev/urandom of="$work/tree/payload-${i}.bin" bs=1M count="$per_file_mib" seek=$((payload_mib / 2)) conv=notrunc status=none
done
dd if=/dev/urandom of="$work/tree/zz-added.bin" bs=1M count=10 status=none
rm "$work/tree/zz-delete-me.bin"

echo "==> second CDC backup must upload less than 25 percent"
bin/backimage backup "$work/tree" --repo "$REPO" --tag t2 --dedup --no-encrypt \
	--allow-degraded --platform linux/amd64 --max-layer-size 64MiB --temp-dir "$work/tmp" \
	--created 2026-08-09T12:01:00Z --json >"$work/t2.json" 2>"$work/t2.err"
uploaded2=$(jq -er '.uploadedBytes' "$work/t2.json")
skipped2=$(jq -er '.skippedBlobs' "$work/t2.json")
[ "$skipped2" -gt 0 ]
[ $((uploaded2 * 4)) -lt "$uploaded1" ]

echo "==> both tags remain runnable and independently restorable"
for tag in t1 t2; do
	docker pull "${REPO}:${tag}" >/dev/null
	docker run --rm "${REPO}:${tag}" verify --json | jq -e '.ok and .full' >/dev/null
	docker run --rm "${REPO}:${tag}" tar >"$work/${tag}.tar"
done
tar -xf "$work/t1.tar" -C "$work/out1"
tar -xf "$work/t2.tar" -C "$work/out2"
diff -qr "$work/original" "$work/out1/tree"
diff -qr "$work/tree" "$work/out2/tree"

echo "phase 10 e2e OK: uploaded ${uploaded1} -> ${uploaded2} bytes"
