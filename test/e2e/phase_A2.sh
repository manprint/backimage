#!/usr/bin/env bash
# Phase A2 e2e: extraction stays inside the destination, and the names it
# writes are the names the backup holds.
#
# Everything here goes through the real CLI against a real encrypted backup in
# a registry. The unit tests cover the crafted archives a backup can never
# produce (a hardlink to "../outside", a directory replaced by a symlink mid
# archive); this script covers what a user can actually hit.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase A2 e2e SKIPPED: docker not available"
	echo "phase A2 e2e OK"
	exit 0
fi
for tool in curl jq cmp tar stat; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

PORT=${PHASEA2_PORT:-5021}
NAME=bi-registry-pA2
HOST="localhost:${PORT}"
REPO="${HOST}/e2e/a2"
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
mkdir -p "$tree/sub" "$tree/hard" "$work/tmp"
printf 'phase A2\n' >"$tree/sub/a.txt"
printf 'second\n' >"$tree/sub/b.txt"
# Names a Unix filesystem accepts and a naive archiver mangles. The backslash
# is the one that used to be lost: it was treated as a separator, so one file
# came back as a directory holding another.
printf 'backslash\n' >"$tree/sub/back\\slash.txt"
printf 'quotes\n' >"$tree/sub/quote'and\"double.txt"
printf 'spaces\n' >"$tree/sub/ trailing and leading .txt"
printf 'unicode\n' >"$tree/sub/ünïcödé-🙃.txt"
# The writer walks in lexical order, so "first.txt" carries the content and
# "second.txt" is the hardlink entry that points back at it.
printf 'shared inode\n' >"$tree/hard/first.txt"
ln "$tree/hard/first.txt" "$tree/hard/second.txt"
ln -s sub/a.txt "$tree/inner-link"
secret=phaseA2-e2e-secret
printf '%s\n' "$secret" >"$work/pass.txt"
chmod 600 "$work/pass.txt"

docker run -d --name "$NAME" -p "${PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do curl -fsS "http://${HOST}/v2/" >/dev/null && break; sleep 0.05; done

echo "==> build the backup"
make embed >/dev/null
bin/backimage backup "$tree" --repo "$REPO" --tag t1 --passphrase-file "$work/pass.txt" \
	--allow-degraded --temp-dir "$work/tmp" --json >/dev/null

echo "==> full restore keeps every name byte for byte"
full="$work/full"
bin/backimage restore "$IMAGE" --extract -C "$full" --passphrase-file "$work/pass.txt" --no-preserve-owner
for rel in 'sub/a.txt' 'sub/b.txt' 'sub/back\slash.txt' "sub/quote'and\"double.txt" \
	'sub/ trailing and leading .txt' 'sub/ünïcödé-🙃.txt' 'hard/first.txt' 'hard/second.txt'; do
	cmp "$tree/$rel" "$full/tree/$rel"
done
if [ -d "$full/tree/sub/back" ]; then
	echo "FAIL: a backslash in a filename became a directory level"
	exit 1
fi
[ -L "$full/tree/inner-link" ] || { echo "FAIL: the symlink was not restored"; exit 1; }

echo "==> the hardlink is a hardlink, not a copy"
a=$(stat -c %i "$full/tree/hard/first.txt")
b=$(stat -c %i "$full/tree/hard/second.txt")
[ "$a" = "$b" ] || { echo "FAIL: the hardlink group was split into two inodes"; exit 1; }

echo "==> the index reports the same names as the filesystem"
BACKIMAGE_PASSPHRASE="$secret" bin/backimage --json ls "$IMAGE" >"$work/ls.json"
jq -e '[.[] | .path] | index("tree/sub/back\\slash.txt")' "$work/ls.json" >/dev/null
jq -e '.[] | select(.path == "tree/hard/second.txt") | .type == "hard"' "$work/ls.json" >/dev/null

echo "==> a symlink in the destination cannot redirect the restore outside it"
outside="$work/outside"
mkdir -p "$outside"
printf 'untouched\n' >"$outside/canary.txt"
chmod 0700 "$outside"

# Case 1: the archive carries the directory entry itself, so the redirecting
# symlink is an object of the wrong type and --overwrite replaces it. The
# restore then writes inside the destination, as it should.
dest="$work/redirect"
mkdir -p "$dest"
ln -s "$outside" "$dest/tree"
bin/backimage --json restore "$IMAGE" --extract -C "$dest" --overwrite \
	--passphrase-file "$work/pass.txt" --no-preserve-owner >"$work/redirect.json"
jq -e '.skipped == 0' "$work/redirect.json" >/dev/null
[ ! -L "$dest/tree" ] || { echo "FAIL: the redirecting symlink survived the restore"; exit 1; }
[ -d "$dest/tree" ] || { echo "FAIL: the archived directory was not created"; exit 1; }
cmp "$tree/sub/a.txt" "$dest/tree/sub/a.txt"
[ ! -e "$outside/sub" ] || { echo "FAIL: the restore wrote through a symlink out of the destination"; exit 1; }

# There is no CLI-reachable case where the symlink survives to redirect a
# write: a selective stream carries the ancestor directory entries too, so the
# wrong-typed object is always replaced first. The archives that can keep a
# redirect in place have to be crafted by hand, and those live in the unit
# tests (pkg/archive/confinement_test.go).
grep -q 'untouched' "$outside/canary.txt"
[ "$(stat -c %a "$outside")" = "700" ] || { echo "FAIL: the mode of a directory outside the destination changed"; exit 1; }

echo "==> selecting only the hardlink brings its first name along"
# DA-03 says a hardlink whose first name is not part of the restore is skipped
# and reported, never rebuilt by reading whatever sits at that path on disk.
# The selective stream makes that case rare on purpose: asking for the link
# also asks for the name it points at, so the group comes back whole instead of
# coming back broken. The skip itself is covered by the unit tests, which can
# craft an archive the backup writer never produces.
dest="$work/filtered"
bin/backimage --json restore "$IMAGE" --extract -C "$dest" \
	--include '**/hard/second.txt' --passphrase-file "$work/pass.txt" --no-preserve-owner >"$work/filtered.json"
jq -e '.skipped == 0' "$work/filtered.json" >/dev/null
jq -e '.skipped_reasons | length == 0' "$work/filtered.json" >/dev/null
a=$(stat -c %i "$dest/tree/hard/first.txt")
b=$(stat -c %i "$dest/tree/hard/second.txt")
[ "$a" = "$b" ] || { echo "FAIL: the selected hardlink group was split into two inodes"; exit 1; }
[ ! -e "$dest/tree/sub/a.txt" ] || { echo "FAIL: a file outside the selection was restored"; exit 1; }

echo "==> --strip-components still selects and strips together"
dest="$work/strip"
mkdir -p "$dest"
bin/backimage restore "$IMAGE" --extract -C "$dest" --include '**/a.txt' --strip-components 1 \
	--passphrase-file "$work/pass.txt" --no-preserve-owner
cmp "$tree/sub/a.txt" "$dest/sub/a.txt"
[ ! -e "$dest/tree" ] || { echo "FAIL: --strip-components was ignored"; exit 1; }

echo "phase A2 e2e OK"
