#!/usr/bin/env bash
# Phase A3 e2e: nothing unverified reaches the consumer, and reading a backup
# costs one pass per layer instead of one per chunk.
#
# No registry and no docker here: every case works on an OCI layout on disk,
# which is also the path where the layer cache is always disabled and the
# per-chunk rebuild hurt most.
set -euo pipefail
cd "$(dirname "$0")/../.."

for tool in cmp stat tar; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

work=$(mktemp -d)
cleanup() {
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
mkdir -p "$work/tmp" "$XDG_CACHE_HOME"
secret=phaseA3-e2e-secret
printf '%s\n' "$secret" >"$work/pass.txt"
chmod 600 "$work/pass.txt"

echo "==> build the swap fixture"
make embed >/dev/null
# Eight 1 MiB blocks, each opening with 64 distinct bytes and zero after that.
# A Rabin fingerprint over a window of zeros is constant, so the content
# defined chunker cuts at exactly its minimum every time: every chunk is 1 MiB,
# which is what makes one chunk substitutable for another. --compression none
# keeps the stored size equal to the plaintext size plus a fixed envelope, so
# the sizes stay equal too. --max-layer-size pins the number of data layers:
# the default boundary is content defined, so with the file mtime changing on
# every run the fixture would sometimes have one layer and sometimes two.
tree="$work/tree"
mkdir -p "$tree"
python3 - "$tree/blocks.bin" <<'PY'
import sys
block = 1 << 20
with open(sys.argv[1], "wb") as f:
	for k in range(8):
		f.write(bytes([(k * 7 + i) % 251 for i in range(64)]))
		f.write(b"\x00" * (block - 64))
PY
bin/backimage backup "$tree" --repo local/a3 --tag t1 \
	--output oci-layout --output-path "$work/layout" \
	--dedup --compression none --runnable=false --allow-degraded \
	--max-layer-size 4MiB \
	--passphrase-file "$work/pass.txt" --temp-dir "$work/tmp" --json >/dev/null

echo "==> the intact backup restores byte for byte"
bin/backimage restore local/a3:t1 --oci-layout "$work/layout" \
	-o - --passphrase-file "$work/pass.txt" -q >"$work/control.tar"
bin/backimage restore local/a3:t1 --oci-layout "$work/layout" --extract \
	-C "$work/control" --passphrase-file "$work/pass.txt" --no-preserve-owner -q
cmp "$tree/blocks.bin" "$work/control/tree/blocks.bin"

echo "==> a validly sealed chunk moved to another position is refused before it is written"
# The chunk table lives inside the sealed private blob, so the offset at which
# the restore must stop is printed by the tool, which holds the passphrase.
stop=$(go run ./test/e2e/tools/forgeclear -layout "$work/layout" -root "$work/unpacked" \
	-passphrase-file "$work/pass.txt" -swap 1:7 \
	-out-layout "$work/swapped" -out-ref local/a3:swapped)
[ "$stop" -gt 0 ] || { echo "FAIL: the tool reported a stop offset of $stop"; exit 1; }

set +e
bin/backimage restore local/a3:swapped --oci-layout "$work/swapped" \
	-o - --passphrase-file "$work/pass.txt" -q >"$work/swapped.tar" 2>"$work/swapped.err"
rc=$?
set -e
[ "$rc" -eq 5 ] || { echo "FAIL: the swapped backup exited $rc, want 5"; cat "$work/swapped.err"; exit 1; }
grep -q "plaintext digest mismatch" "$work/swapped.err" || {
	echo "FAIL: the refusal did not come from the plaintext digest"; cat "$work/swapped.err"; exit 1; }
got=$(stat -c %s "$work/swapped.tar")
[ "$got" -eq "$stop" ] || {
	echo "FAIL: the consumer received $got bytes, want exactly the $stop of the chunks before the substituted one"
	exit 1
}
# What it did receive must be the real thing, not a truncated re-encoding.
head -c "$stop" "$work/control.tar" >"$work/prefix.tar"
cmp "$work/prefix.tar" "$work/swapped.tar"

echo "==> the same rebuild without a swap still restores"
go run ./test/e2e/tools/forgeclear -layout "$work/layout" -root "$work/unpacked-ctl" \
	-passphrase-file "$work/pass.txt" -forge "" \
	-out-layout "$work/rebuilt" -out-ref local/a3:rebuilt >/dev/null
bin/backimage restore local/a3:rebuilt --oci-layout "$work/rebuilt" \
	-o - --passphrase-file "$work/pass.txt" -q >"$work/rebuilt.tar"
cmp "$work/control.tar" "$work/rebuilt.tar"

echo "==> reading a multi-layer backup leaves no materialised layer behind"
# An OCI layout never keeps a layer cache, so every data layer is materialised
# into a temporary file. It must be built once per layer and removed when the
# source closes, not rebuilt and dropped once per chunk.
#
# This fixture drops --dedup on purpose: with it the layer boundary is content
# defined and probabilistic, so the number of layers changes from run to run.
# Without it the boundary is a fixed size. The floor of a layer is 16 MiB, so
# the source has to be a comfortable multiple of it for the count to be stable.
multi="$work/multitree"
mkdir -p "$multi"
head -c 50331648 /dev/zero >"$multi/flat.bin"
bin/backimage backup "$multi" --repo local/a3multi --tag t1 \
	--output oci-layout --output-path "$work/multilayout" --allow-degraded \
	--max-layer-size 16MiB --compression none --runnable=false \
	--passphrase-file "$work/pass.txt" --temp-dir "$work/tmp" --json >/dev/null
layers=$(bin/backimage inspect local/a3multi:t1 --oci-layout "$work/multilayout" --json | grep -o '"chunkFrom"' | wc -l)
[ "$layers" -ge 2 ] || { echo "FAIL: the fixture has $layers data layers, the test needs at least 2"; exit 1; }
rm -rf "$XDG_CACHE_HOME"/backimage
bin/backimage restore local/a3multi:t1 --oci-layout "$work/multilayout" --extract \
	-C "$work/multidest" --passphrase-file "$work/pass.txt" --no-preserve-owner -q
cmp "$multi/flat.bin" "$work/multidest/multitree/flat.bin"
left=$(find "$XDG_CACHE_HOME" -name '.layer-*' 2>/dev/null | wc -l)
[ "$left" -eq 0 ] || { echo "FAIL: $left materialised layers survived the restore"; exit 1; }

echo "==> a partial recovery holds one chunk, not one entry"
if ! command -v /usr/bin/time >/dev/null 2>&1; then
	echo "  SKIPPED: /usr/bin/time is not available, cannot measure resident memory"
else
	big="$work/big"
	mkdir -p "$big"
	head -c 1073741824 /dev/zero >"$big/huge.bin"
	bin/backimage backup "$big" --repo local/a3big --tag t1 \
		--output oci-layout --output-path "$work/biglayout" --allow-degraded \
		--passphrase-file "$work/pass.txt" --temp-dir "$work/tmp" --json >/dev/null
	# --continue takes the partial path, the one that used to collect a whole
	# entry before writing it: a 1 GiB file inside the backup meant more than
	# 2 GiB resident. The limit below is above what the rest of the process
	# needs and well under the size of the entry.
	/usr/bin/time -f '%M' -o "$work/rss.txt" \
		bin/backimage restore local/a3big:t1 --oci-layout "$work/biglayout" \
		--continue -o - --passphrase-file "$work/pass.txt" -q >"$work/big.tar"
	rss=$(cat "$work/rss.txt")
	echo "  peak resident: ${rss} KiB for a 1 GiB entry"
	[ "$rss" -lt 1048576 ] || {
		echo "FAIL: the partial recovery peaked at ${rss} KiB, more than the 1 GiB entry it was writing"
		exit 1
	}
	# And it must have written the entry, not skipped it.
	tar -tf "$work/big.tar" >/dev/null
	[ "$(stat -c %s "$work/big.tar")" -gt 1073741824 ] || {
		echo "FAIL: the partial recovery did not write the whole entry"; exit 1; }
fi

echo "phase A3 e2e OK"
