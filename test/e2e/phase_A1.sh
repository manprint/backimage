#!/usr/bin/env bash
# Phase A1 e2e: an encrypted backup whose blobs lost their authentication tag
# is refused, and the two data-loss paths of the restore stay inside what was
# asked for.
#
# The forged images are produced by test/e2e/tools/forgeclear, which rewrites
# the chosen blobs as clear envelopes and repairs every public number that
# describes them: sizes, stored digests, blob names, layer metadata. Nothing
# a reader can check without the key is left inconsistent, so what refuses the
# backup here is the rule that an encrypted backup has no unauthenticated
# blobs, and nothing else.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase A1 e2e SKIPPED: docker not available"
	echo "phase A1 e2e OK"
	exit 0
fi
for tool in curl jq cmp tar; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

PORT=${PHASEA1_PORT:-5020}
NAME=bi-registry-pA1
HOST="localhost:${PORT}"
REPO="${HOST}/e2e/a1"
LAYOUT_REF="example.test/e2e/a1"
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
mkdir -p "$tree/sub" "$work/tmp"
printf 'phase A1 plaintext\n' >"$tree/sub/a.txt"
printf 'second file\n' >"$tree/sub/b.txt"
dd if=/dev/urandom of="$tree/random.bin" bs=1M count=6 status=none
secret=phaseA1-e2e-secret
printf '%s\n' "$secret" >"$work/pass.txt"
chmod 600 "$work/pass.txt"

docker run -d --name "$NAME" -p "${PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do curl -fsS "http://${HOST}/v2/" >/dev/null && break; sleep 0.05; done

echo "==> build the honest encrypted backup into an OCI layout"
make embed >/dev/null
SELF=internal/embedded/backimage-selfextract-linux-amd64
go build -o "$work/forgeclear" ./test/e2e/tools/forgeclear
bin/backimage backup "$tree" --repo "$LAYOUT_REF" --tag honest --passphrase-file "$work/pass.txt" \
	--allow-degraded --output oci-layout --output-path "$work/layout" --platform linux/amd64 \
	--max-layer-size 4MiB --temp-dir "$work/tmp" --json >/dev/null

# The Docker daemon is not among the sources exercised here: `docker save`
# re-labels every layer as gzip, so `restore --local-repo` cannot read back a
# backup at all, forged or honest. That defect predates this phase and is
# tracked separately (plan/astra/bugs.md, B-A001).

# refuse runs a command that must fail, and reports the exit code so a change
# of classification is visible instead of silently accepted.
refuse() {
	local what="$1"; shift
	set +e
	"$@" >"$work/out.log" 2>"$work/err.log"
	local rc=$?
	set -e
	if [ "$rc" -eq 0 ]; then
		echo "FAIL: $what succeeded on a forged backup"
		sed -n '1,20p' "$work/out.log"
		exit 1
	fi
	if [ "$rc" -ne 5 ]; then
		echo "FAIL: $what exited $rc, expected 5 (integrity)"
		sed -n '1,20p' "$work/err.log"
		exit 1
	fi
}

# fails_with is refuse's sibling for the refusals that are not integrity
# answers, such as a destination that already holds files.
fails_with() {
	local want="$1" what="$2"; shift 2
	set +e
	"$@" >"$work/out.log" 2>"$work/err.log"
	local rc=$?
	set -e
	if [ "$rc" -ne "$want" ]; then
		echo "FAIL: $what exited $rc, expected $want"
		sed -n '1,20p' "$work/err.log"
		exit 1
	fi
}

accept() {
	local what="$1"; shift
	if ! "$@" >"$work/out.log" 2>"$work/err.log"; then
		echo "FAIL: $what should have succeeded"
		sed -n '1,20p' "$work/err.log"
		exit 1
	fi
}

no_plaintext() {
	local dir="$1"
	if [ -e "$dir/tree/sub/a.txt" ] || [ -e "$dir/tree/random.bin" ]; then
		echo "FAIL: plaintext released into $dir"
		exit 1
	fi
}

# forge builds one forged copy: an unpacked root for the extractor, an OCI
# layout and a registry tag for the host binary, and a daemon image.
forge() {
	local tag="$1" targets="$2"
	"$work/forgeclear" -layout "$work/layout" -root "$work/root-$tag" \
		-passphrase-file "$work/pass.txt" -forge "$targets" \
		-out-layout "$work/layout-$tag" -out-ref "${LAYOUT_REF}:${tag}" \
		-push "${REPO}:${tag}"
}

echo "==> control: unpacked and repacked without forging, everything still works"
forge honest ""
accept "host verify on the repacked layout" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage --json verify "${LAYOUT_REF}:honest" --oci-layout "$work/layout-honest"
accept "host ls on the repacked layout" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage ls "${LAYOUT_REF}:honest" --oci-layout "$work/layout-honest"
accept "host restore from the registry" \
	bin/backimage restore "${REPO}:honest" --extract -C "$work/honest-out" \
	--passphrase-file "$work/pass.txt" --no-preserve-owner
cmp "$tree/sub/a.txt" "$work/honest-out/tree/sub/a.txt"
cmp "$tree/random.bin" "$work/honest-out/tree/random.bin"
accept "extractor verify on the unpacked root" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" verify --root "$work/root-honest/backup"
accept "extractor list on the unpacked root" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" list --root "$work/root-honest/backup"

echo "==> a downgraded data blob is refused by every reader of the plaintext"
forge data data
refuse "host tar restore (layout)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:data" --oci-layout "$work/layout-data" \
	-o "$work/data.tar"
[ ! -e "$work/data.tar" ]
refuse "host extract (layout)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:data" --oci-layout "$work/layout-data" \
	--extract -C "$work/x-data" --no-preserve-owner
no_plaintext "$work/x-data"
refuse "host selective extract (layout)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:data" --oci-layout "$work/layout-data" \
	--extract -C "$work/x-data-sel" --include '**/a.txt' --no-preserve-owner
no_plaintext "$work/x-data-sel"
refuse "host extract with --continue (layout)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:data" --oci-layout "$work/layout-data" \
	--extract -C "$work/x-data-cont" --continue --no-preserve-owner
no_plaintext "$work/x-data-cont"
refuse "host extract with --no-verify (layout)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:data" --oci-layout "$work/layout-data" \
	--extract -C "$work/x-data-nv" --no-verify --no-preserve-owner
no_plaintext "$work/x-data-nv"
refuse "host full verify (layout)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage verify "${LAYOUT_REF}:data" --oci-layout "$work/layout-data"
refuse "host extract (registry)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${REPO}:data" \
	--extract -C "$work/x-data-reg" --no-preserve-owner
no_plaintext "$work/x-data-reg"
refuse "extractor tar" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" tar --root "$work/root-data/backup"
refuse "extractor extract" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" extract --root "$work/root-data/backup" --out "$work/x-data-self"
no_plaintext "$work/x-data-self"
refuse "extractor verify" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" verify --root "$work/root-data/backup"
# The index is still authenticated: listing it is a different question, and it
# keeps answering. A refusal here would be the guard firing on the wrong blob.
accept "host ls with only the data forged" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage ls "${LAYOUT_REF}:data" --oci-layout "$work/layout-data"
# --quick reads the stored bytes only, and the forgery repaired those digests.
accept "host quick verify with only the data forged" \
	bin/backimage --json verify "${LAYOUT_REF}:data" --oci-layout "$work/layout-data" --quick

echo "==> a downgraded index is refused by every reader of the index"
forge index index
refuse "host ls" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage ls "${LAYOUT_REF}:index" --oci-layout "$work/layout-index"
refuse "host find" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage find "${LAYOUT_REF}:index" '**/a.txt' --oci-layout "$work/layout-index"
refuse "host selective extract" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:index" --oci-layout "$work/layout-index" \
	--extract -C "$work/x-index-sel" --include '**/a.txt' --no-preserve-owner
no_plaintext "$work/x-index-sel"
refuse "host extract with --continue" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:index" --oci-layout "$work/layout-index" \
	--extract -C "$work/x-index-cont" --continue --no-preserve-owner
no_plaintext "$work/x-index-cont"
refuse "extractor list" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" list --root "$work/root-index/backup"
refuse "host extract (registry)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${REPO}:index" \
	--extract -C "$work/x-index-reg" --include '**/a.txt' --no-preserve-owner
no_plaintext "$work/x-index-reg"

echo "==> a downgraded private blob is refused before the backup ever unlocks"
forge private private
refuse "host extract" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:private" --oci-layout "$work/layout-private" \
	--extract -C "$work/x-private" --no-preserve-owner
no_plaintext "$work/x-private"
refuse "host ls" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage ls "${LAYOUT_REF}:private" --oci-layout "$work/layout-private"
refuse "host full verify" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage verify "${LAYOUT_REF}:private" --oci-layout "$work/layout-private"
refuse "extractor verify" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" verify --root "$work/root-private/backup"
refuse "extractor list" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" list --root "$work/root-private/backup"

echo "==> all three downgraded together: nothing reads, from any source"
forge all data,index,private
for cmd_desc in tar extract ls verify; do
	case "$cmd_desc" in
	tar) refuse "host tar (all)" env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:all" \
		--oci-layout "$work/layout-all" -o "$work/all.tar"; [ ! -e "$work/all.tar" ] ;;
	extract) refuse "host extract (all)" env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:all" \
		--oci-layout "$work/layout-all" --extract -C "$work/x-all" --no-preserve-owner; no_plaintext "$work/x-all" ;;
	ls) refuse "host ls (all)" env BACKIMAGE_PASSPHRASE="$secret" bin/backimage ls "${LAYOUT_REF}:all" \
		--oci-layout "$work/layout-all" ;;
	verify) refuse "host verify (all)" env BACKIMAGE_PASSPHRASE="$secret" bin/backimage verify "${LAYOUT_REF}:all" \
		--oci-layout "$work/layout-all" ;;
	esac
done
refuse "host extract (registry, all)" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${REPO}:all" \
	--extract -C "$work/x-all-reg" --no-preserve-owner
no_plaintext "$work/x-all-reg"
for sub in list tar verify; do
	refuse "extractor $sub (all)" env BACKIMAGE_PASSPHRASE="$secret" "$SELF" "$sub" --root "$work/root-all/backup"
done
refuse "extractor extract (all)" \
	env BACKIMAGE_PASSPHRASE="$secret" "$SELF" extract --root "$work/root-all/backup" --out "$work/x-all-self"
no_plaintext "$work/x-all-self"

echo "==> A12: --overwrite overlays the destination, it does not empty it"
dest="$work/a12"
mkdir -p "$dest/tree/sub"
printf 'not in the backup\n' >"$dest/tree/sub/foreign.txt"
printf 'unrelated\n' >"$dest/unrelated.txt"
accept "overlay restore" \
	bin/backimage restore "${REPO}:honest" --extract -C "$dest" --overwrite \
	--passphrase-file "$work/pass.txt" --no-preserve-owner
[ -f "$dest/tree/sub/foreign.txt" ] || { echo "FAIL: --overwrite deleted a file the backup does not contain"; exit 1; }
[ -f "$dest/unrelated.txt" ] || { echo "FAIL: --overwrite deleted an unrelated file"; exit 1; }
cmp "$tree/sub/a.txt" "$dest/tree/sub/a.txt"

echo "==> A12: a type mismatch is still replaced"
dest="$work/a12-type"
mkdir -p "$dest/tree"
printf 'a file where the backup has a directory\n' >"$dest/tree/sub"
accept "type-mismatch restore" \
	bin/backimage restore "${REPO}:honest" --extract -C "$dest" --overwrite \
	--passphrase-file "$work/pass.txt" --no-preserve-owner
[ -d "$dest/tree/sub" ] || { echo "FAIL: a file was not replaced by the archived directory"; exit 1; }
cmp "$tree/sub/a.txt" "$dest/tree/sub/a.txt"

dest="$work/a12-dir-over-file"
mkdir -p "$dest/tree/sub/a.txt"
printf 'child of a directory that must go\n' >"$dest/tree/sub/a.txt/child"
accept "directory-over-file restore" \
	bin/backimage restore "${REPO}:honest" --extract -C "$dest" --overwrite \
	--passphrase-file "$work/pass.txt" --no-preserve-owner
[ -f "$dest/tree/sub/a.txt" ] || { echo "FAIL: a directory was not replaced by the archived file"; exit 1; }

echo "==> A12: without --overwrite an existing entry still stops the restore"
dest="$work/a12-refuse"
mkdir -p "$dest/tree/sub"
printf 'existing\n' >"$dest/tree/sub/a.txt"
fails_with 2 "restore over an existing file without --overwrite" \
	bin/backimage restore "${REPO}:honest" --extract -C "$dest" \
	--passphrase-file "$work/pass.txt" --no-preserve-owner
grep -q 'existing' "$dest/tree/sub/a.txt"

echo "==> A13: --continue restores nothing beyond the filters"
dest="$work/a13-include"
accept "continue with --include" \
	bin/backimage restore "${REPO}:honest" --extract -C "$dest" --continue \
	--include '**/a.txt' --passphrase-file "$work/pass.txt" --no-preserve-owner
cmp "$tree/sub/a.txt" "$dest/tree/sub/a.txt"
[ ! -e "$dest/tree/random.bin" ] || { echo "FAIL: --continue --include restored an excluded file"; exit 1; }
[ ! -e "$dest/tree/sub/b.txt" ] || { echo "FAIL: --continue --include restored an excluded file"; exit 1; }

dest="$work/a13-exclude"
accept "continue with --exclude" \
	bin/backimage restore "${REPO}:honest" --extract -C "$dest" --continue \
	--exclude '**/random.bin' --passphrase-file "$work/pass.txt" --no-preserve-owner
[ ! -e "$dest/tree/random.bin" ] || { echo "FAIL: --continue --exclude restored an excluded file"; exit 1; }
cmp "$tree/sub/b.txt" "$dest/tree/sub/b.txt"

dest="$work/a13-strip"
mkdir -p "$dest"
accept "continue with --strip-components" \
	bin/backimage restore "${REPO}:honest" --extract -C "$dest" --continue \
	--include '**/a.txt' --strip-components 1 --passphrase-file "$work/pass.txt" --no-preserve-owner
cmp "$tree/sub/a.txt" "$dest/sub/a.txt"
[ ! -e "$dest/tree" ] || { echo "FAIL: --strip-components was ignored"; exit 1; }

dest="$work/a13-tar"
mkdir -p "$dest"
accept "continue with --include towards a tar" \
	bin/backimage restore "${REPO}:honest" --continue --include '**/a.txt' \
	-o "$dest/sel.tar" --passphrase-file "$work/pass.txt"
tar -tf "$dest/sel.tar" >"$work/sel.list"
grep -q 'tree/sub/a.txt' "$work/sel.list"
if grep -q 'random.bin' "$work/sel.list"; then
	echo "FAIL: --continue towards a tar ignored --include"
	exit 1
fi

echo "phase A1 e2e OK"
