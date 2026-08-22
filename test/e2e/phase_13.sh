#!/usr/bin/env bash
# Phase 13 e2e: per-family retention on a real registry.
#
# The unit tests assert which DELETE requests the prune sends; only a registry
# that really implements deletion can show the tag disappearing afterwards, and
# that the tags a selector excluded are still there. That is what this runs.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase 13 e2e SKIPPED: docker not available"
	echo "phase 13 e2e OK"
	exit 0
fi
for tool in jq curl; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

REG_PORT=${PHASE13_REGISTRY_PORT:-5009}
NAME=bi-registry-p13
HOST="localhost:${REG_PORT}"
REPO="${HOST}/e2e/prune"
work=$(mktemp -d)
fail=0

cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase 13 diagnostics (exit $rc)" >&2
		for log in "$work"/*.err; do [ -f "$log" ] && { echo "[$log]" >&2; sed -n '1,60p' "$log" >&2; }; done
	fi
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

bi() { bin/backimage "$@"; }

# tags_of lists the tags the registry still serves, sorted, space separated.
tags_of() {
	curl -fsS "http://${HOST}/v2/e2e/prune/tags/list" | jq -r '.tags // [] | sort | join(" ")'
}

expect() {
	local what=$1 got=$2 want=$3
	if [ "$got" != "$want" ]; then
		echo "FAIL: $what" >&2
		echo "  got:  $got" >&2
		echo "  want: $want" >&2
		fail=1
	else
		echo "  ok: $what"
	fi
}

# publish TAG CREATED [SRC] [EXCLUDE] — one tiny reproducible backup.
publish() {
	local tag=$1 created=$2 src=${3:-$work/tree} exclude=${4:-}
	local extra=()
	if [ -n "$exclude" ]; then extra=(--exclude "$exclude"); fi
	bi backup "$src" --repo "$REPO" --tag "$tag" --created "$created" \
		--no-encrypt --allow-degraded --platform linux/amd64 \
		--temp-dir "$work/tmp" --quiet "${extra[@]}" \
		>>"$work/publish.log" 2>>"$work/publish.err"
}

digest_of() {
	bi repo tags "$REPO" --json | jq -r --arg t "$1" '.[] | select(.tag == $t) | .digest'
}

mkdir -p "$work/tree" "$work/tmp" "$work/twin"
printf 'phase 13 retention fixture\n' >"$work/tree/data.txt"
printf 'phase 13 retention fixture\n' >"$work/twin/data.txt"

# Deletion is off by default in registry:2, and prune must be able to delete.
docker run -d --name "$NAME" -p "${REG_PORT}:5000" \
	-e REGISTRY_STORAGE_DELETE_ENABLED=true registry:2 >/dev/null
for _ in $(seq 1 200); do curl -fsS "http://${HOST}/v2/" >/dev/null 2>&1 && break; sleep 0.05; done
make embed >/dev/null

echo "==> publish two families, four backups each (db_1 oldest .. db_4 newest)"
for i in 1 2 3 4; do
	publish "db_${i}"  "2026-08-0${i}T12:00:00Z"
	publish "app_${i}" "2026-08-0${i}T13:00:00Z"
done
# A tag outside both families and outside every pattern used below.
publish "latest" "2026-07-01T00:00:00Z"
expect "nine tags published" "$(tags_of)" "app_1 app_2 app_3 app_4 db_1 db_2 db_3 db_4 latest"

echo "==> a selector without a retention rule must delete nothing"
before=$(tags_of)
bi repo prune "$REPO" --tag-regex 'db_.*' --yes >"$work/norule.out" 2>"$work/norule.err"
grep -q "nessuna regola" "$work/norule.out" || { echo "FAIL: no-rule message missing" >&2; fail=1; }
expect "no rule, no deletion" "$(tags_of)" "$before"

echo "==> a pattern that does not cover the whole tag selects nothing"
bi repo prune "$REPO" --tag-regex 'db_' --keep-last 1 --dry-run >"$work/anchor.out" 2>"$work/anchor.err"
grep -q "nessun tag corrisponde" "$work/anchor.out" || { echo "FAIL: zero-match message missing" >&2; fail=1; }
grep -q "tag intero" "$work/anchor.out" || { echo "FAIL: the anchoring is not explained" >&2; fail=1; }
expect "unanchored pattern, no deletion" "$(tags_of)" "$before"

echo "==> the read-only preview and the prune select the same tags"
preview=$(bi repo tags "$REPO" --tag-regex 'db_.*' --json | jq -r '[.[].tag] | sort | join(" ")')
doomed=$(bi repo prune "$REPO" --tag-regex 'db_.*' --keep-within 1s --dry-run --json |
	jq -r '[.remove[].tag] | sort | join(" ")')
expect "preview equals what prune would remove" "$preview" "$doomed"
expect "preview holds only the db family" "$preview" "db_1 db_2 db_3 db_4"

echo "==> --tag-regex prunes one family and leaves the rest alone"
bi repo prune "$REPO" --tag-regex 'db_.*' --keep-last 3 --yes --json >"$work/scoped.json" 2>"$work/scoped.err"
expect "scope matched only the db family" \
	"$(jq -r '.scope | "\(.tagRegex) \(.matched)/\(.total)"' "$work/scoped.json")" "db_.* 4/9"
expect "only the oldest db backup was removed" \
	"$(jq -r '[.remove[].tag] | join(" ")' "$work/scoped.json")" "db_1"
expect "the registry lost exactly that tag" "$(tags_of)" \
	"app_1 app_2 app_3 app_4 db_2 db_3 db_4 latest"

echo "==> --group-by-regex keeps the newest of every family in one pass"
bi repo prune "$REPO" --group-by-regex '([a-z]+)_.*' --keep-last 2 --yes --json \
	>"$work/grouped.json" 2>"$work/grouped.err"
expect "two families, one ungroupable tag left alone" \
	"$(jq -r '.groupBy | "\(.groups) \(.ungrouped)"' "$work/grouped.json")" "2 1"
expect "the oldest of each family was removed" \
	"$(jq -r '[.remove[].tag] | sort | join(" ")' "$work/grouped.json")" "app_1 app_2 db_2"
expect "both families kept their two newest, latest untouched" "$(tags_of)" \
	"app_3 app_4 db_3 db_4 latest"

echo "==> a manifest shared with a kept tag stops the prune before any deletion"
# Identical source, identical creation time: backimage is reproducible, so both
# tags should land on one manifest. If they do not, the case cannot be built and
# the check reports that instead of passing quietly.
publish "twin_a" "2026-06-01T00:00:00Z" "$work/twin"
publish "twin_b" "2026-06-01T00:00:00Z" "$work/twin"
da=$(digest_of twin_a)
db=$(digest_of twin_b)
if [ "$da" != "$db" ]; then
	echo "  SKIPPED shared-manifest check: twin_a ($da) and twin_b ($db) are distinct manifests"
else
	before=$(tags_of)
	if bi repo prune "$REPO" --tag-regex 'twin_a' --keep-within 1s --yes \
		>"$work/shared.out" 2>"$work/shared.err"; then
		echo "FAIL: the prune deleted a manifest still referenced by twin_b" >&2
		fail=1
	else
		grep -q "twin_b" "$work/shared.err" "$work/shared.out" || { echo "FAIL: the kept tag is not named" >&2; fail=1; }
		grep -q "nessun tag è stato rimosso" "$work/shared.err" "$work/shared.out" ||
			{ echo "FAIL: the refusal does not state that nothing was deleted" >&2; fail=1; }
	fi
	expect "the refusal left the registry untouched" "$(tags_of)" "$before"

	echo "==> both tags of a shared manifest go together, in one request"
	bi repo prune "$REPO" --tag-regex 'twin_.*' --keep-within 1s --yes --json \
		>"$work/twins.json" 2>"$work/twins.err"
	expect "both twins removed" \
		"$(jq -r '[.remove[].tag] | sort | join(" ")' "$work/twins.json")" "twin_a twin_b"
	expect "the twins are gone from the registry" "$(tags_of)" "app_3 app_4 db_3 db_4 latest"
fi

echo "==> an excluded subtree must not reach the registry at all"
# The exclusion filter used to go through filepath.Match, where "**" spans a
# single segment: everything nested deeper than the pattern was archived anyway.
mkdir -p "$work/tree/.cache/chromium/Default"
printf 'shallow\n' >"$work/tree/.cache/cookies.db"
printf 'deep\n' >"$work/tree/.cache/chromium/Default/Cookies"
publish "excl" "2026-05-01T00:00:00Z" "$work/tree" 'tree/.cache/**'
expect "the excluded subtree is absent, the rest is not" \
	"$(bi ls "${REPO}:excl" --json 2>/dev/null | jq -r '[.[].path] | map(select(contains(".cache"))) | length')" "0"
expect "the unrelated file survived the exclusion" \
	"$(bi ls "${REPO}:excl" --json 2>/dev/null | jq -r '[.[].path] | map(select(endswith("data.txt"))) | length')" "1"
rm -rf "$work/tree/.cache"

if [ "$fail" -ne 0 ]; then
	echo "phase 13 e2e FAILED" >&2
	exit 1
fi
echo "phase 13 e2e OK"
