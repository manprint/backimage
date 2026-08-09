#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./cmd/gendocs -out "${1:-docs/cli.md}"
