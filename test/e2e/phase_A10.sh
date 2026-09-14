#!/usr/bin/env bash
# Phase A10 e2e: the registry lifecycle commands nothing ran, against a
# registry that really implements deletion.
#
#  repo caps    which operations the adapter offers, by name
#  repo stats   shared blobs and effective storage across tags
#  repo rm      an irreversible deletion, and the two gates in front of it
#  logout       removing one stored account, or all of them
#  --cache-size the downloaded-layer cache is used, and 0 really disables it
#
# Unit tests assert which requests each of these sends. Only a real registry
# can show the tag gone afterwards, the blobs actually shared between two tags,
# and the cache file appearing on disk — which is what this runs.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
	echo "phase A10 e2e SKIPPED: docker not available"
	echo "phase A10 e2e OK"
	exit 0
fi
for tool in jq curl; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

PORT=${PHASEA10_PORT:-5052}
NAME=bi-registry-pA10
HOST="localhost:${PORT}"
REPO="${HOST}/e2e/a10"
work=$(mktemp -d)
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase A10 diagnostics (exit $rc)" >&2
		for log in "$work"/last.log "$work"/last.out; do
			[ -f "$log" ] && { echo "[$log]" >&2; sed -n '1,30p' "$log" >&2; }
		done
	fi
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
mkdir -p "$work/tree/sub" "$work/tmp"
printf 'phase A10\n' >"$work/tree/sub/a.txt"
dd if=/dev/urandom of="$work/tree/random.bin" bs=1M count=3 status=none

# Deletion is off by default in registry:2, and repo rm must be able to delete.
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p "${PORT}:5000" \
	-e REGISTRY_STORAGE_DELETE_ENABLED=true registry:2 >/dev/null
for _ in $(seq 1 200); do curl -fsS "http://${HOST}/v2/" >/dev/null 2>&1 && break; sleep 0.05; done
make embed >/dev/null
go build -o bin/backimage ./cmd/backimage

# fails_with WANT WHAT COMMAND... — stdout and stderr kept apart.
fails_with() {
	local want="$1" what="$2"; shift 2
	set +e
	"$@" >"$work/last.out" 2>"$work/last.log"
	local rc=$?
	set -e
	[ "$rc" = "$want" ] || { echo "FAIL: $what è uscito $rc, atteso $want"; sed -n '1,20p' "$work/last.log"; exit 1; }
}

# publish TAG [extra args...] — --created fixed so two tags of the same tree
# land on the same manifest, which is what the --force gate is about.
publish() {
	local tag=$1; shift
	bin/backimage backup "$work/tree" --repo "$REPO" --tag "$tag" --no-encrypt \
		--allow-degraded --platform linux/amd64 --temp-dir "$work/tmp" --quiet "$@" \
		>"$work/last.out" 2>"$work/last.log"
}

tags_of() { curl -fsS "http://${HOST}/v2/e2e/a10/tags/list" | jq -r '.tags // [] | sort | join(" ")'; }

# ---------------------------------------------------------------------------
# A10.1 — repo caps names the operations
# ---------------------------------------------------------------------------
bin/backimage repo caps "$HOST" --json >"$work/caps.json" 2>"$work/last.log"
jq -e '.capabilities | type == "array"' "$work/caps.json" >/dev/null || {
	echo "FAIL: repo caps --json non elenca le operazioni"; cat "$work/caps.json"; exit 1; }
for op in list-tags delete-manifest delete-tag usage-stats; do
	jq -e --arg o "$op" '.capabilities | index($o) != null' "$work/caps.json" >/dev/null || {
		echo "FAIL: repo caps non dichiara $op"; cat "$work/caps.json"; exit 1; }
done
jq -e '.adapter == "oci"' "$work/caps.json" >/dev/null || { echo "FAIL: adapter sbagliato"; exit 1; }
# The bitmask must not come back: it named no operation at all, and --json was
# ignored outright, so both forms are checked.
if grep -q 'capabilities:[0-9]' "$work/caps.json"; then
	echo "FAIL: repo caps stampa ancora la bitmask"; exit 1
fi
bin/backimage repo caps "$HOST" >"$work/caps.txt" 2>"$work/last.log"
grep -q 'delete-manifest' "$work/caps.txt" || {
	echo "FAIL: la forma testuale di repo caps non nomina le operazioni"; cat "$work/caps.txt"; exit 1; }
if grep -qE 'map\[|capabilities:[0-9]' "$work/caps.txt"; then
	echo "FAIL: la forma testuale di repo caps stampa una mappa Go"; cat "$work/caps.txt"; exit 1
fi
echo "A10.1 repo caps: operazioni per nome, in JSON e in testo: OK"

# ---------------------------------------------------------------------------
# A10.2 — repo stats sees what two tags share
# ---------------------------------------------------------------------------
publish one --created 2026-03-01T00:00:00Z
bin/backimage repo stats "$REPO" --json >"$work/stats1.json" 2>"$work/last.log"
jq -e '.tags == 1 and .sharedBlobs == 0' "$work/stats1.json" >/dev/null || {
	echo "FAIL: con un solo tag non ci può essere nulla di condiviso"; cat "$work/stats1.json"; exit 1; }
publish two --created 2026-03-02T00:00:00Z
bin/backimage repo stats "$REPO" --json >"$work/stats2.json" 2>"$work/last.log"
jq -e '.tags == 2 and .sharedBlobs > 0' "$work/stats2.json" >/dev/null || {
	echo "FAIL: due backup dello stesso albero non condividono blob"; cat "$work/stats2.json"; exit 1; }
# Deduplication is the whole point of the number: what the registry stores has
# to be less than the sum over the tags.
jq -e '.storageBytes < .referencedBytes' "$work/stats2.json" >/dev/null || {
	echo "FAIL: storage non minore dei byte riferiti"; cat "$work/stats2.json"; exit 1; }
bin/backimage repo stats "$REPO" >"$work/stats.txt" 2>"$work/last.log"
grep -q 'condivisi' "$work/stats.txt" || { echo "FAIL: la forma testuale non riporta i blob condivisi"; cat "$work/stats.txt"; exit 1; }
echo "A10.2 repo stats: 2 tag, blob condivisi, storage < byte riferiti: OK"

# ---------------------------------------------------------------------------
# A10.3 — repo rm: the gates come before the deletion
# ---------------------------------------------------------------------------
before=$(tags_of)
fails_with 2 "repo rm senza --yes" bin/backimage repo rm "$REPO:one"
grep -q -- '--yes' "$work/last.log" || { echo "FAIL: il rifiuto non nomina --yes"; cat "$work/last.log"; exit 1; }
[ "$(tags_of)" = "$before" ] || { echo "FAIL: repo rm ha cancellato senza --yes"; exit 1; }

# Two tags on one manifest: deleting either removes both, so the command has to
# say so instead of doing it. It is a refusal to act, not a transport failure,
# so it is a usage error (2) and not a network one (6): a script must not retry it.
publish twinA --created 2026-04-01T00:00:00Z
publish twinB --created 2026-04-01T00:00:00Z
twin_a=$(bin/backimage repo tags "$REPO" --json | jq -r '.[] | select(.tag == "twinA") | .digest')
twin_b=$(bin/backimage repo tags "$REPO" --json | jq -r '.[] | select(.tag == "twinB") | .digest')
[ -n "$twin_a" ] && [ "$twin_a" = "$twin_b" ] || {
	echo "FAIL: i due tag gemelli non puntano allo stesso manifest ($twin_a vs $twin_b)"; exit 1; }
fails_with 2 "repo rm di un manifest condiviso" bin/backimage repo rm "$REPO:twinA" --yes
grep -q -- '--force' "$work/last.log" || { echo "FAIL: il rifiuto non indica --force"; cat "$work/last.log"; exit 1; }
case "$(tags_of)" in *twinA*twinB*) ;; *) echo "FAIL: il rifiuto ha comunque cancellato"; exit 1;; esac
echo "A10.3 repo rm: senza --yes e su un manifest condiviso rifiuta con exit 2, senza cancellare: OK"

# ---------------------------------------------------------------------------
# A10.4 — repo rm deletes, and --force takes the shared tags together
# ---------------------------------------------------------------------------
bin/backimage repo rm "$REPO:one" --yes >"$work/rm.txt" 2>"$work/last.log"
grep -q "eliminato ${REPO}:one" "$work/rm.txt" || {
	echo "FAIL: la conferma della cancellazione non è leggibile"; cat "$work/rm.txt"; exit 1; }
if grep -q 'map\[' "$work/rm.txt"; then
	echo "FAIL: repo rm conferma con una mappa Go"; cat "$work/rm.txt"; exit 1
fi
case "$(tags_of)" in *one*) echo "FAIL: il tag è ancora servito dopo la cancellazione"; exit 1;; esac
case "$(tags_of)" in *two*) ;; *) echo "FAIL: la cancellazione ha portato via anche gli altri tag"; exit 1;; esac

bin/backimage repo rm "$REPO:twinA" --yes --force --json >"$work/rmf.json" 2>"$work/last.log"
jq -e --arg r "${REPO}:twinA" '.deleted == $r' "$work/rmf.json" >/dev/null || {
	echo "FAIL: repo rm --json non conferma la cancellazione"; cat "$work/rmf.json"; exit 1; }
# --force was warned about for a reason: both twins are gone.
case "$(tags_of)" in *twin*) echo "FAIL: --force ha lasciato uno dei due tag gemelli: $(tags_of)"; exit 1;; esac

fails_with 6 "repo rm di un tag inesistente" bin/backimage repo rm "$REPO:non-esiste" --yes
echo "A10.4 repo rm: cancella il tag scelto, --force porta via i gemelli, un tag assente è un errore: OK"

# ---------------------------------------------------------------------------
# A10.5 — --cache-size on a real download
# ---------------------------------------------------------------------------
publish cached --created 2026-05-01T00:00:00Z
layers_dir="$XDG_CACHE_HOME/backimage/layers"
rm -rf "$XDG_CACHE_HOME/backimage" "$work/out-cache"
bin/backimage restore "$REPO:cached" -x -C "$work/out-cache" --no-preserve-owner \
	>"$work/last.out" 2>"$work/last.log"
cached_files=$(find "$layers_dir" -type f 2>/dev/null | wc -l)
[ "$cached_files" -ge 1 ] || { echo "FAIL: nessun layer messo in cache dal restore"; exit 1; }
rm -rf "$XDG_CACHE_HOME/backimage" "$work/out-nocache"
bin/backimage restore "$REPO:cached" -x -C "$work/out-nocache" --no-preserve-owner \
	--cache-size 0 >"$work/last.out" 2>"$work/last.log"
zero_files=$(find "$layers_dir" -type f 2>/dev/null | wc -l)
[ "$zero_files" = "0" ] || {
	echo "FAIL: --cache-size 0 ha comunque lasciato $zero_files file in cache"; exit 1; }
cmp "$work/tree/random.bin" "$work/out-cache/tree/random.bin"
cmp "$work/tree/random.bin" "$work/out-nocache/tree/random.bin"
echo "A10.5 --cache-size: la cache si popola ($cached_files layer), con 0 resta vuota, l'albero è lo stesso: OK"

# ---------------------------------------------------------------------------
# A10.6 — logout
# ---------------------------------------------------------------------------
printf 'pw1\n' | bin/backimage login "$HOST" -u utente1 --password-stdin >/dev/null 2>"$work/last.log"
printf 'pw2\n' | bin/backimage login "$HOST" -u utente2 --password-stdin >/dev/null 2>"$work/last.log"
[ "$(stat -c '%a' "$BACKIMAGE_AUTH_FILE")" = 600 ] || { echo "FAIL: auth.json non è 0600"; exit 1; }
[ "$(jq -r '.auths | length' "$BACKIMAGE_AUTH_FILE")" = "2" ] || {
	echo "FAIL: i due account non sono stati memorizzati"; jq -c '.auths | keys' "$BACKIMAGE_AUTH_FILE"; exit 1; }
# With more than one account the command must not guess which to remove.
fails_with 2 "logout ambiguo" bin/backimage logout "$HOST"
if ! grep -q 'utente1' "$work/last.log" || ! grep -q 'utente2' "$work/last.log"; then
	echo "FAIL: il rifiuto non elenca gli account"; cat "$work/last.log"; exit 1
fi
[ "$(jq -r '.auths | length' "$BACKIMAGE_AUTH_FILE")" = "2" ] || { echo "FAIL: il logout ambiguo ha rimosso qualcosa"; exit 1; }
bin/backimage logout "$HOST" --user utente1 >"$work/last.out" 2>"$work/last.log"
[ "$(jq -r '.auths | length' "$BACKIMAGE_AUTH_FILE")" = "1" ] || {
	echo "FAIL: --user non ha rimosso esattamente un account"; jq -c '.auths | keys' "$BACKIMAGE_AUTH_FILE"; exit 1; }
bin/backimage logout "$HOST" --all >"$work/last.out" 2>"$work/last.log"
[ "$(jq -r '.auths | length' "$BACKIMAGE_AUTH_FILE")" = "0" ] || {
	echo "FAIL: --all ha lasciato credenziali"; jq -c '.auths | keys' "$BACKIMAGE_AUTH_FILE"; exit 1; }
# Nothing stored is not an error: a logout has to be safe to repeat.
bin/backimage logout "$HOST" >"$work/last.out" 2>"$work/last.log"
[ "$(stat -c '%a' "$BACKIMAGE_AUTH_FILE")" = 600 ] || { echo "FAIL: auth.json non è più 0600 dopo il logout"; exit 1; }
echo "A10.6 logout: rifiuta l'ambiguità, --user ne toglie uno, --all li toglie tutti, ripeterlo è innocuo: OK"

echo "phase A10 e2e OK"
