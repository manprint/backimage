#!/usr/bin/env bash
# Freeze one complete backup per released on-disk format under
# pkg/recovery/testdata/, so a change to the writer can never take away the
# ability to read what earlier releases wrote.
#
# Run it deliberately, never from a gate: it rewrites committed fixtures.
# A fixture is regenerated only when a NEW released format has to be frozen;
# regenerating an existing one silently replaces the very evidence the
# compatibility tests rest on.
#
# An existing fixture is never overwritten: the script refuses and says so.
# Pass --force only to rebuild one you have just deleted on purpose.
#
#   bash scripts/make-format-fixtures.sh            # the missing ones
#   bash scripts/make-format-fixtures.sh schema2-envelope3
#
# schema1-plain, schema2-encrypted and legacy-envelope1 cannot be reproduced
# by this tree any more: their headers carry envelope versions this build no
# longer writes. That is the point of having frozen them.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT=pkg/recovery/testdata
PASS='fixture-passphrase'
# Fixed so the manifest of a regenerated fixture differs only where the
# format differs.
CREATED='2026-01-02T03:04:05Z'
LEGACY_TAG=v0.2.3-dev.3

FORCE=0
args=()
for arg in "$@"; do
	if [ "$arg" = "--force" ]; then FORCE=1; else args+=("$arg"); fi
done
set -- "${args[@]+"${args[@]}"}"

want() { [ "$#" -eq 0 ] && return 0; case " $* " in *" $FIXTURE "*) return 0;; esac; return 1; }

work=$(mktemp -d)
worktree=
cleanup() {
	rc=$?
	[ -n "$worktree" ] && git worktree remove --force "$worktree" >/dev/null 2>&1
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

mkdir -p "$work/src/sub" "$work/tmp"
printf 'fixture payload\n' >"$work/src/hello.txt"
printf 'nested\n' >"$work/src/sub/nested.txt"
ln -s sub/nested.txt "$work/src/link"
chmod 0640 "$work/src/sub/nested.txt"
printf '%s\n' "$PASS" >"$work/pass"
chmod 600 "$work/pass"

go build -o "$work/unpackbackup" ./test/e2e/tools/unpackbackup

# generate NAME BINARY EXTRA_FLAGS...
generate() {
	local name=$1 bin=$2; shift 2
	local layout="$work/layout-$name"
	if [ -d "$OUT/$name" ] && [ "$FORCE" -ne 1 ]; then
		echo "kept $OUT/$name (already frozen; --force to replace it)" >&2
		return 0
	fi
	rm -rf "$layout" "$OUT/$name"
	"$bin" backup "$work/src" --repo "fixtures.invalid/format/$name" --tag frozen \
		--output oci-layout --output-path "$layout" \
		--runnable=false --platform linux/amd64 --allow-degraded \
		--created "$CREATED" --temp-dir "$work/tmp" "$@" >/dev/null
	mkdir -p "$OUT/$name"
	"$work/unpackbackup" -layout "$layout" -out "$work/tree-$name"
	mv "$work/tree-$name/backup"/* "$OUT/$name/"
	echo "frozen $OUT/$name"
}

selected=("$@")
run_one() {
	FIXTURE=$1
	shift
	if want "${selected[@]+"${selected[@]}"}"; then "$@"; fi
}

go build -o "$work/backimage" ./cmd/backimage

run_one schema1-plain generate schema1-plain "$work/backimage" --no-encrypt
run_one schema2-encrypted generate schema2-encrypted "$work/backimage" --passphrase-file "$work/pass"
# The format this build writes: envelope 3, attested key material, metadata
# bound inside the sealed blob.
run_one schema2-envelope3 generate schema2-envelope3 "$work/backimage" --passphrase-file "$work/pass"

FIXTURE=legacy-envelope1
if want "${selected[@]+"${selected[@]}"}"; then
	worktree="$work/legacy"
	git worktree add --detach "$worktree" "$LEGACY_TAG" >/dev/null
	# The old build needs its own embedded extractor: the image builder asks
	# for it even with --runnable=false.
	(
		cd "$worktree"
		for arch in amd64 arm64; do
			rm -f "internal/embedded/backimage-selfextract-linux-$arch"
			GOOS=linux GOARCH=$arch go build \
				-o "internal/embedded/backimage-selfextract-linux-$arch" \
				./cmd/backimage-selfextract
		done
		go build -o "$work/backimage-legacy" ./cmd/backimage
	)
	generate legacy-envelope1 "$work/backimage-legacy" --passphrase-file "$work/pass"
	git worktree remove --force "$worktree" >/dev/null
	worktree=
fi

cat <<EOF

Fixtures written under $OUT. The passphrase of the encrypted ones is
"$PASS"; pkg/recovery/format_compat_test.go holds the same constant.
EOF
