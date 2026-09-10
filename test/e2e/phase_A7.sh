#!/usr/bin/env bash
# Phase A7 e2e: hostile metadata on a local layout, and what it costs to
# refuse it.
#
# The claim of the phase is not that a rewritten backup is rejected — several
# earlier phases already show that. It is that the rejection happens *before*
# the reader allocates what the rewritten file asked for. An assertion on the
# exit code cannot tell the two apart: a reader that allocates eight gigabytes
# and then notices the blob is smaller exits 5 as well. So every case here is
# measured: the peak resident memory of the process that refused it, taken
# from the kernel's own accounting of the child.
#
# No registry and no docker: the extractor reads a backup directory directly,
# which is exactly the shape a hostile publisher controls.
set -euo pipefail
cd "$(dirname "$0")/../.."

for tool in jq python3; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

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

# The ceiling every hostile case is measured against. The honest run below is
# around 12 MiB, and the smallest declaration these fixtures make is 8 GiB:
# anything in between is a reader that refused for the right reason. 128 MiB
# leaves room for a different allocator, a different Go release and a busier
# machine without ever coming close to the amount being asked for.
CEILING_KIB=$((128 * 1024))
# A refusal that has to read the whole backup first is not a refusal in time.
MAX_SECONDS=30

echo "==> build the honest backup"
make embed >/dev/null
SELF=internal/embedded/backimage-selfextract-linux-amd64
go build -o "$work/backimage" ./cmd/backimage
go build -o "$work/unpackbackup" ./test/e2e/tools/unpackbackup
go build -o "$work/forgeindex" ./test/e2e/tools/forgeindex

tree="$work/tree"
mkdir -p "$tree/sub"
printf 'phase A7 payload\n' >"$tree/a.txt"
# Incompressible, so the stored size of the single chunk is a real number and
# the hostile declarations below are visibly absurd next to it.
python3 - "$tree/sub/big.bin" <<'PY'
import random, sys
random.seed(7)
with open(sys.argv[1], "wb") as f:
	f.write(bytes(random.getrandbits(8) for _ in range(300000)))
PY

# --no-encrypt on purpose: an encrypted backup is refused by the sealed
# binding of A6.3 before any of these numbers is read, and this phase is
# about the numbers.
"$work/backimage" backup "$tree" --repo local/a7 --tag t1 --no-encrypt \
	--output oci-layout --output-path "$work/layout" \
	--runnable=false --platform linux/amd64 --allow-degraded \
	--temp-dir "$work/tmp" >/dev/null
"$work/unpackbackup" -layout "$work/layout" -out "$work/unpacked"
honest="$work/unpacked/backup"
[ -f "$honest/manifest.json" ] || { echo "FAIL: the backup tree was not unpacked"; exit 1; }

# maxrss.py runs a command and prints the peak resident set of the child, in
# KiB, from getrusage(RUSAGE_CHILDREN). Sampling /proc would miss a process
# that refuses in a millisecond, which is precisely the outcome being
# measured.
cat >"$work/maxrss.py" <<'PY'
import resource, subprocess, sys
rc = subprocess.run(sys.argv[1:], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode
print(resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss)
sys.exit(rc)
PY

peak=0
elapsed=0
# measured runs one command, capturing its exit code, peak memory and wall
# clock. It never fails the script itself: every caller states what it wanted.
measured() {
	local started ended
	started=$(date +%s)
	set +e
	peak=$(python3 "$work/maxrss.py" "$@")
	local rc=$?
	set -e
	ended=$(date +%s)
	elapsed=$((ended - started))
	return "$rc"
}

# refused asserts the whole property in one place: exit 5, refused in time,
# and refused without allocating what the fixture asked for.
refused() {
	local name=$1; shift
	local rc=0
	measured "$@" || rc=$?
	if [ "$rc" -ne 5 ]; then
		echo "FAIL: $name exited $rc, want 5 (integrity)"
		"$@" || true
		exit 1
	fi
	if [ "$peak" -gt "$CEILING_KIB" ]; then
		echo "FAIL: $name refused, but allocated ${peak} KiB doing it (ceiling ${CEILING_KIB} KiB)"
		exit 1
	fi
	if [ "$elapsed" -gt "$MAX_SECONDS" ]; then
		echo "FAIL: $name took ${elapsed}s to refuse (limit ${MAX_SECONDS}s)"
		exit 1
	fi
	echo "    $name: refused, ${peak} KiB, ${elapsed}s"
}

# forged copies the honest tree and gives back the copy.
forged() {
	local name=$1
	rm -rf "$work/$name"
	cp -r "$honest" "$work/$name"
	printf '%s\n' "$work/$name"
}

echo "==> the honest backup verifies, and that is the baseline"
measured "$SELF" verify --root "$honest" || { echo "FAIL: the honest backup does not verify"; exit 1; }
baseline=$peak
echo "    baseline: ${baseline} KiB"
if [ "$baseline" -gt "$CEILING_KIB" ]; then
	echo "FAIL: the honest run already needs ${baseline} KiB, the hostile ceiling says nothing"
	exit 1
fi

echo "==> a chunk that declares more than the layer holding it"
# The chunk table alone lies. Nothing else is touched, so the manifest already
# contradicts it: the two public files have to agree before either is used.
case1=$(forged case-chunk-vs-manifest)
jq '.chunks[0].sb = 8589934592' "$honest/chunks.json" >"$case1/chunks.json"
refused "chunk larger than its layer" "$SELF" verify --root "$case1"

echo "==> a backup whose public files agree with each other and not with the blob"
# The harder shape: chunks.json, the layer size and the declared chunk size
# are all rewritten together, so every cross-check between the public files
# passes. What is left is the blob on disk, and its real size is the only
# authority that cannot be rewritten from the metadata.
case2=$(forged case-blob-is-the-authority)
jq '.chunks[0].sb = 8589934592' "$honest/chunks.json" >"$case2/chunks.json"
jq '.layers[0].storedBytes = 8589934592 | .chunking.targetChunkBytes = 8589934592' \
	"$honest/manifest.json" >"$case2/manifest.json"
refused "stored size past the real blob" "$SELF" verify --root "$case2"

echo "==> an extraction of the same backup writes nothing"
dest="$work/extracted"
mkdir -p "$dest"
refused "extract of a lying backup" "$SELF" extract --root "$case2" --out "$dest" --no-preserve-owner
if [ -n "$(ls -A "$dest")" ]; then
	echo "FAIL: the refused extraction left files behind: $(ls -A "$dest")"
	exit 1
fi

echo "==> an index whose shape its readers already assume"
for shape in duplicate-path backwards-offsets long-path; do
	dir=$(forged "case-index-$shape")
	"$work/forgeindex" -root "$dir" -shape "$shape" >/dev/null
	refused "index $shape" "$SELF" verify --root "$dir"
done

echo "==> the honest backup is still honest"
# The fixtures above are copies, so this is not a tautology: it states that
# nothing in this phase made a sound backup unreadable.
"$SELF" verify --root "$honest" >/dev/null || { echo "FAIL: the honest backup stopped verifying"; exit 1; }
"$SELF" list --root "$honest" >/dev/null || { echo "FAIL: the honest backup stopped listing"; exit 1; }

echo "phase A7 e2e OK"
