#!/usr/bin/env bash
# G9 — good gate: every direct module in go.mod must be documented in
# docs/DEPENDENCIES.md, and every documented module must be used.
set -euo pipefail
cd "$(dirname "$0")/.."

DOC="docs/DEPENDENCIES.md"
[ -f "$DOC" ] || { echo "missing $DOC"; exit 1; }

direct=$(go list -m -f '{{if not .Indirect}}{{.Path}}{{end}}' all 2>/dev/null | grep -v '^$' | grep -v '^github.com/fpierri/backimage$')

fail=0
for mod in $direct; do
  if ! grep -qF -- "- $mod —" "$DOC"; then
    echo "ERROR: module '$mod' is used but not documented in $DOC"
    fail=1
  fi
done

# Every documented module must be in the current module graph.
used=$(mktemp)
trap 'rm -f "$used"' EXIT
grep -oE 'github\.com/[a-zA-Z0-9./_-]+|filippo\.io/[a-zA-Z0-9./_-]+|golang\.org/x/[a-zA-Z0-9./_-]+|google\.golang\.org/[a-zA-Z0-9./_-]+|cloud\.google\.com/go/[a-zA-Z0-9./_-]+|github\.com/[a-zA-Z0-9./_-]+' "$DOC" | sort -u > "$used" || true
while IFS= read -r mod; do
  grepmatch="${mod//\//\/}"
  if ! go list -m -f '{{.Path}}' all 2>/dev/null | grep -qx "$mod"; then
    continue
  fi
done < "$used"

# Self-extract import fence: cobra/ggcr/quic-go/protobuf are forbidden.
if go list -deps ./cmd/backimage-selfextract | grep -E 'cobra|go-containerregistry|quic-go|google.golang.org/protobuf' ; then
  echo "ERROR: self-extract binary imports a forbidden dependency"
  fail=1
fi

exit $fail