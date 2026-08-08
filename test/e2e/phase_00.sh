#!/usr/bin/env bash
# Phase 00 e2e smoke: build and run version.
set -euo pipefail
cd "$(dirname "$0")/../.."
make build >/dev/null
out=$(bin/backimage version --json)
echo "$out" | jq -e '.version' >/dev/null
out2=$(bin/backimage version)
echo "$out2" | grep -q "backimage version"
echo "phase 00 e2e OK"
