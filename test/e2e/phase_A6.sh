#!/usr/bin/env bash
# Phase A6 e2e: what decides whether a deduplication key may seal again, and
# what the sealed metadata says about the public files served next to it.
#
# Three claims, all through the real CLI against a real registry:
#
#   A05 — only the attestation inside the age blob decides key reuse. A public
#         manifest that lies in either direction changes nothing: it cannot
#         resurrect a burned key, and it cannot burn a sound one. Rotation is
#         asked for with --rotate-key, announced, and costs one full upload.
#   A20 — the sealed metadata names the manifest, the chunk table and the index
#         blob of its own backup. A composition whose counts all agree is
#         refused before a single byte is emitted.
#   The frozen fixtures of every released format still open with the extractor
#   this tree ships, not only with the Go compatibility tests.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase A6 e2e SKIPPED: docker not available"
	echo "phase A6 e2e OK"
	exit 0
fi
for tool in curl jq diff; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

PORT=${PHASEA6_PORT:-5061}
NAME=bi-registry-pA6
HOST="localhost:${PORT}"
REPO_D="${HOST}/e2e/a6-dedup"
REPO_L="${HOST}/e2e/a6-lying"
REPO_K="${HOST}/e2e/a6-legacy"
LAYOUT_REF="fixtures.invalid/a6"
work=$(mktemp -d)
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase A6 diagnostics (exit $rc)" >&2
		for log in "$work"/*.err; do
			[ -f "$log" ] || continue
			echo "[$(basename "$log")]" >&2
			sed -n '1,40p' "$log" >&2
		done
	fi
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"

tree="$work/tree"
mkdir -p "$tree/sub" "$work/tmp"
printf 'phase A6 payload\n' >"$tree/sub/a.txt"
printf 'second file\n' >"$tree/b.txt"
# Incompressible, so the byte measurements below say something: a fixture that
# stores two kilobytes cannot show the cost of a rotation.
dd if=/dev/urandom of="$tree/random.bin" bs=1M count=8 status=none
secret=phaseA6-e2e-secret
printf '%s\n' "$secret" >"$work/pass"
chmod 600 "$work/pass"

docker run -d --name "$NAME" -p "${PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do curl -fsS "http://${HOST}/v2/" >/dev/null && break; sleep 0.05; done

make embed >/dev/null
SELF=internal/embedded/backimage-selfextract-linux-amd64
go build -o "$work/forgeclear" ./test/e2e/tools/forgeclear

# backup_to runs one backup and leaves its json report in $work/$name.json and
# its progress lines in $work/$name.err.
backup_to() {
	local name=$1; shift
	bin/backimage backup "$tree" --passphrase-file "$work/pass" --allow-degraded \
		--platform linux/amd64 --max-layer-size 4MiB --temp-dir "$work/tmp" \
		--json "$@" >"$work/$name.json" 2>"$work/$name.err"
}

# fingerprint reads the key fingerprint of a backup. It lives in the sealed
# metadata, so it is the one field an attacker who rewrites the public files
# cannot touch, and two backups share it exactly when they share a key.
fingerprint() {
	bin/backimage inspect "$1" --json --passphrase-file "$work/pass" "${@:2}" \
		| jq -er '.manifest.encryption.keyFingerprint'
}

echo "==> the format this build writes: envelope 3, schema 2, convergent nonces"
backup_to t1 --repo "$REPO_D" --tag t1 --dedup
bin/backimage inspect "${REPO_D}:t1" --json --passphrase-file "$work/pass" >"$work/t1.inspect.json"
jq -e '.manifest.schemaVersion == 2
	and .manifest.encryption.envelopeVersion == 3
	and .manifest.encryption.nonceMode == "convergent"
	and (.manifest.private.path | length) > 0' "$work/t1.inspect.json" >/dev/null \
	|| { echo "FAIL: the new format is not what the manifest declares"; jq . "$work/t1.inspect.json"; exit 1; }

echo "==> and it roundtrips: verify, then a restore that matches the source"
bin/backimage verify "${REPO_D}:t1" --json --passphrase-file "$work/pass" | jq -e '.ok' >/dev/null
bin/backimage restore "${REPO_D}:t1" --extract -C "$work/out-t1" \
	--passphrase-file "$work/pass" --no-preserve-owner >/dev/null
diff -r "$tree" "$work/out-t1/tree" >/dev/null

echo "==> a second dedup backup reuses the key and shares its blobs"
backup_to t2 --repo "$REPO_D" --tag t2 --dedup
skipped2=$(jq -er '.skippedBytes' "$work/t2.json")
[ "$skipped2" -gt 0 ] || { echo "FAIL: the second dedup backup shared nothing"; exit 1; }
fp1=$(fingerprint "${REPO_D}:t1")
fp2=$(fingerprint "${REPO_D}:t2")
[ "$fp1" = "$fp2" ] || { echo "FAIL: the key was not reused ($fp1 vs $fp2)"; exit 1; }
if grep -q 'nuova chiave\|non viene riusata' "$work/t2.err"; then
	echo "FAIL: a sound key was refused"; sed -n '1,20p' "$work/t2.err"; exit 1
fi

echo "==> --rotate-key is asked for, announced, and pays one full upload"
backup_to t3 --repo "$REPO_D" --tag t3 --dedup --rotate-key
grep -q -- '--rotate-key' "$work/t3.err" || { echo "FAIL: the rotation was not announced"; exit 1; }
grep -q 'ricarica tutti i blob' "$work/t3.err" || { echo "FAIL: the cost was not announced"; exit 1; }
fp3=$(fingerprint "${REPO_D}:t3")
[ "$fp3" != "$fp1" ] || { echo "FAIL: --rotate-key kept the previous key"; exit 1; }
uploaded1=$(jq -er '.uploadedBytes' "$work/t1.json")
uploaded3=$(jq -er '.uploadedBytes' "$work/t3.json")
[ "$uploaded3" -gt $((uploaded1 / 2)) ] \
	|| { echo "FAIL: a rotated key must re-upload the data ($uploaded3 against $uploaded1)"; exit 1; }

echo "==> and deduplication is normal again from the next backup"
backup_to t4 --repo "$REPO_D" --tag t4 --dedup
[ "$(fingerprint "${REPO_D}:t4")" = "$fp3" ] || { echo "FAIL: the rotated key was not reused"; exit 1; }
[ "$(jq -er '.skippedBytes' "$work/t4.json")" -gt 0 ] \
	|| { echo "FAIL: dedup did not resume after a rotation"; exit 1; }

echo "==> build a layout to forge from: one honest backup, key file included"
backup_to layout --repo "$LAYOUT_REF" --tag base --dedup \
	--output oci-layout --output-path "$work/layout"
base_fp=$(fingerprint "${LAYOUT_REF}:base" --oci-layout "$work/layout")

echo "==> a lying public manifest cannot burn a sound key (A05, both directions)"
# envelopeVersion and nonceMode are public planning hints. Rewritten to values
# that would refuse reuse if they were believed, they must change nothing: the
# age blob still attests a current, reusable, convergent key.
"$work/forgeclear" -layout "$work/layout" -root "$work/root-lying" \
	-set-envelope-version 1 -set-nonce-mode random -push "${REPO_L}:base" >/dev/null
backup_to lying --repo "$REPO_L" --tag next --dedup
if grep -q 'nuova chiave\|non viene riusata' "$work/lying.err"; then
	echo "FAIL: a public field was believed over the attestation"
	sed -n '1,20p' "$work/lying.err"; exit 1
fi
[ "$(fingerprint "${REPO_L}:next")" = "$base_fp" ] \
	|| { echo "FAIL: a rewritten manifest cost a full re-upload"; exit 1; }

echo "==> key material from before the attestation is never reused (A05)"
# What a backup written by an older release looks like: the same secrets, the
# same passphrase, and an age blob that does not say which epoch made it.
"$work/forgeclear" -layout "$work/layout" -root "$work/root-legacy" \
	-passphrase-file "$work/pass" -downgrade-key legacy -push "${REPO_K}:base" >/dev/null
backup_to legacy --repo "$REPO_K" --tag next --dedup
grep -q 'epoca crittografica' "$work/legacy.err" \
	|| { echo "FAIL: the refusal did not name the missing attestation"; sed -n '1,20p' "$work/legacy.err"; exit 1; }
grep -q 'ricarica tutti i blob' "$work/legacy.err" \
	|| { echo "FAIL: the cost of the refusal was not announced"; exit 1; }
[ "$(fingerprint "${REPO_K}:next")" != "$base_fp" ] \
	|| { echo "FAIL: key material with no attestation sealed again"; exit 1; }

# refuse_integrity runs a restore that must fail as an integrity answer, and
# proves that nothing was written on the way out.
refuse_integrity() {
	local what=$1 dest=$2; shift 2
	set +e
	"$@" >"$work/$what.out" 2>"$work/$what.err"
	local rc=$?
	set -e
	[ "$rc" -eq 5 ] || {
		echo "FAIL: $what exited $rc, expected 5 (integrity)"
		sed -n '1,20p' "$work/$what.err"; exit 1
	}
	[ ! -e "$dest" ] || { echo "FAIL: $what wrote something into $dest"; exit 1; }
}

echo "==> the sealed metadata refuses a rewritten manifest (A20)"
"$work/forgeclear" -layout "$work/layout" -root "$work/root-manifest" \
	-set-envelope-version 2 \
	-out-layout "$work/layout-manifest" -out-ref "${LAYOUT_REF}:manifest" >/dev/null
refuse_integrity forged-manifest "$work/out-manifest" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:manifest" \
	--oci-layout "$work/layout-manifest" --extract -C "$work/out-manifest" --no-preserve-owner
grep -q 'envelopeVersion' "$work/forged-manifest.err" \
	|| { echo "FAIL: the refusal did not name the field that was rewritten"; exit 1; }

echo "==> and a chunk table repaired so every count still agrees (A20)"
# The only cross-check that ever existed was that the chunk counts matched.
# This edit keeps them matching, and keeps everything else matching too: two
# entries trade their stored digest and nothing else, so every number in the
# file — the count, each stored size, each layer total — is exactly what it
# was. Only the sealed binding can tell that this is not the chunk table of
# this backup.
#
# The earlier version of this fixture also traded the stored sizes. That was
# fine until A7.1, which cross-checks the per-layer totals against the
# manifest before anything else: whenever the two chosen entries happened to
# live in different layers — and with content-defined boundaries that is a
# coin toss between runs — the sums moved and the refusal came from the
# layer check instead of from the binding. Also a correct refusal, but not
# the one this case exists to measure. Swapping only the digest cannot move
# a byte between layers.
table="$work/root-lying/backup/chunks.json"
before=$(jq -er '.chunks | length' "$table")
[ "$before" -ge 2 ] || { echo "FAIL: the fixture has $before chunks, too few to compose anything"; exit 1; }
# The first entry whose stored digest differs from the first chunk's: two
# identical digests are the same deduplicated blob, and trading them would
# leave the file byte for byte as it was.
other=$(jq -er 'first(.chunks | to_entries[] | select(.key > 0 and .value.ss != $c0) | .key)' \
	--arg c0 "$(jq -er '.chunks[0].ss' "$table")" "$table" 2>/dev/null || true)
[ -n "$other" ] || { echo "FAIL: every chunk of the fixture has the same stored digest, nothing to trade"; exit 1; }
jq --argjson j "$other" '.chunks as $c
	| .chunks = ($c
		| .[0]  = ($c[0]  + {ss: $c[$j].ss})
		| .[$j] = ($c[$j] + {ss: $c[0].ss}))' \
	"$table" >"$work/chunks-repaired.json"
[ "$(jq -er '.chunks | length' "$work/chunks-repaired.json")" = "$before" ] \
	|| { echo "FAIL: the fixture changed the chunk count, so it proves nothing"; exit 1; }
if cmp -s "$table" "$work/chunks-repaired.json"; then
	echo "FAIL: the forged chunk table is identical to the honest one"
	exit 1
fi
[ "$(jq -erS '[.chunks[].sb]' "$table")" = "$(jq -erS '[.chunks[].sb]' "$work/chunks-repaired.json")" ] \
	|| { echo "FAIL: the forge moved stored bytes, so the layer totals refuse before the binding"; exit 1; }
"$work/forgeclear" -layout "$work/layout" -root "$work/root-chunks" \
	-graft-chunks "$work/chunks-repaired.json" \
	-out-layout "$work/layout-chunks" -out-ref "${LAYOUT_REF}:chunks" >/dev/null
refuse_integrity forged-chunks "$work/out-chunks" \
	env BACKIMAGE_PASSPHRASE="$secret" bin/backimage restore "${LAYOUT_REF}:chunks" \
	--oci-layout "$work/layout-chunks" --extract -C "$work/out-chunks" --no-preserve-owner
grep -q 'chunks.json' "$work/forged-chunks.err" \
	|| { echo "FAIL: the refusal did not name the file that does not belong"; exit 1; }

echo "==> every frozen format still opens with the extractor this tree ships"
found=0
for fixture in pkg/recovery/testdata/*/; do
	[ -f "$fixture/manifest.json" ] || continue
	found=$((found + 1))
	name=$(basename "$fixture")
	if jq -e '.encryption.enabled' "$fixture/manifest.json" >/dev/null; then
		env BACKIMAGE_PASSPHRASE=fixture-passphrase "$SELF" verify --root "$fixture" >/dev/null \
			|| { echo "FAIL: the shipped extractor cannot verify the frozen fixture $name"; exit 1; }
		env BACKIMAGE_PASSPHRASE=fixture-passphrase "$SELF" list --root "$fixture" >/dev/null \
			|| { echo "FAIL: the shipped extractor cannot list the frozen fixture $name"; exit 1; }
	else
		"$SELF" verify --root "$fixture" >/dev/null \
			|| { echo "FAIL: the shipped extractor cannot verify the frozen fixture $name"; exit 1; }
	fi
	echo "    $name: readable"
done
[ "$found" -ge 4 ] || { echo "FAIL: only $found frozen fixtures were found, expected every released format"; exit 1; }

echo "phase A6 e2e OK"
