#!/usr/bin/env bash
# G10 — documentation gate check:
#  1. every deliverable doc of every completed phase (per plan/resume.md) exists;
#  2. every `backimage <subcommand>` cited in README.md exists in bin/backimage.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0

declare -A PHASE_DOCS=(
  [00]="README.md docs/DEPENDENCIES.md docs/BUILD.md docs/ARCHITECTURE.md docs/CONTRIBUTING.md CHANGELOG.md"
  [01]="docs/FIDELITY.md"
  [02]="docs/compression.md"
  [03]="docs/security.md"
  [04]="docs/image-format.md"
  [05]="docs/backup.md docs/registries.md"
  [06]="docs/selfextract.md"
  [07]="docs/restore.md docs/cli.md"
  [08]="docs/remote.md docs/protocol.md"
  [09]="docs/transport-benchmark.md"
  [10]="docs/dedup.md"
  [11]="docs/retention.md"
  [12]="docs/troubleshooting.md docs/FAQ.md"
)

resume="plan/resume.md"
[ -f "$resume" ] || { echo "ERROR: plan/resume.md missing"; exit 1; }

phase_done() {
  grep -qE "\- \[[x]\] .*\*\*Gate fase $1\*\*" "$resume"
}

for phase in 00 01 02 03 04 05 06 07 08 09 10 11 12; do
  if phase_done "$phase"; then
    for f in ${PHASE_DOCS[$phase]}; do
      if [ ! -f "$f" ]; then
        echo "FAIL: deliverable doc '$f' (phase $phase) missing"
        fail=1
      fi
    done
  fi
done

if phase_done 07 && [ -f docs/cli.md ]; then
  generated=$(mktemp)
  trap 'rm -f "$generated"' EXIT
  bash scripts/generate-cli-docs.sh "$generated"
  if ! cmp -s docs/cli.md "$generated"; then
    echo "FAIL: docs/cli.md is stale; run scripts/generate-cli-docs.sh"
    fail=1
  fi
fi

if [ -f README.md ] && [ -x bin/backimage ]; then
  cmds=$(grep -oE 'backimage [a-z][a-z-]+' README.md | awk '{print $2}' | sort -u || true)
  for c in $cmds; do
    if ! bin/backimage --help 2>&1 | grep -qw "$c"; then
      echo "FAIL: README cites 'backimage $c' which is not a known subcommand"
      fail=1
    fi
  done
fi

exit $fail
