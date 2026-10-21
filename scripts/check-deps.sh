#!/usr/bin/env bash
# G9 — good gate: every direct module in go.mod must be documented in
# docs/DEPENDENCIES.md, and every documented module must be used.
set -euo pipefail
cd "$(dirname "$0")/.."

DOC="docs/DEPENDENCIES.md"
[ -f "$DOC" ] || { echo "missing $DOC"; exit 1; }

direct=$(go list -m -f '{{if not .Indirect}}{{.Path}}{{end}}' all 2>/dev/null | grep -v '^$' | grep -v '^github.com/manprint/backimage$')

fail=0
for mod in $direct; do
  if ! grep -qF -- "- $mod —" "$DOC"; then
    echo "ERROR: module '$mod' is used but not documented in $DOC"
    fail=1
  fi
done

# Every documented module must be in the current module graph: a stale entry
# documents a dependency the build no longer has. Only the list entries count
# ("- MODULE — purpose — phase"), not module paths quoted in the prose.
graph=$(go list -m -f '{{.Path}}' all)
documented=$(sed -n 's/^- \([^ ]*\) — .*/\1/p' "$DOC")
for mod in $documented; do
  if ! grep -qxF -- "$mod" <<<"$graph"; then
    echo "ERROR: module '$mod' is documented in $DOC but not in the module graph"
    fail=1
  fi
done

# Self-extract import fence: cobra/ggcr/quic-go/protobuf are forbidden, and so
# is the Docker client. The extractor runs inside the image being restored;
# reaching the daemon socket from there is control of the host far beyond
# deleting one image, so the package must not even be linked in.
if go list -deps ./cmd/backimage-selfextract | grep -E 'cobra|go-containerregistry|quic-go|google.golang.org/protobuf|github.com/manprint/backimage/pkg/docker' ; then
  echo "ERROR: self-extract binary imports a forbidden dependency"
  fail=1
fi

# Self-extract size budget (docs/DEPENDENCIES.md): every image carries the
# extractor in layer 0. The embedded assets are measured, so the check needs
# `make selfextract` first, as `make check` and CI run it; the committed
# placeholders are far below the budget.
budget=8000000
for asset in internal/embedded/backimage-selfextract-linux-*; do
  [ -f "$asset" ] || continue
  size=$(wc -c < "$asset")
  if [ "$size" -gt "$budget" ]; then
    echo "ERROR: $asset is $size bytes, over the $budget-byte self-extract budget"
    fail=1
  fi
done

exit $fail