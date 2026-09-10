#!/usr/bin/env bash
# Regenerates pkg/protocol/backimage.pb.go in a scratch directory and fails on
# drift. Three outcomes, deliberately distinct:
#
#   toolchain present, no drift    -> exit 0, "protobuf generated sources are current"
#   toolchain present, drift       -> exit != 0 (cmp reports the difference)
#   toolchain absent               -> exit 0 with a SKIP line, unless
#                                     BACKIMAGE_REQUIRE_PROTOC=1 is set
#
# The skip exists so `make check` is runnable on a developer machine without
# protoc: a gate that is always red is a gate that is always ignored. CI sets
# BACKIMAGE_REQUIRE_PROTOC=1, so a missing toolchain there is a failure and the
# check can never be silently lost.
set -euo pipefail
cd "$(dirname "$0")/.."

gopath_bin="$(go env GOPATH 2>/dev/null || true)/bin"

protoc_bin=${PROTOC:-}
if [ -z "$protoc_bin" ] && [ -x /tmp/backimage-protoc/bin/protoc ]; then protoc_bin=/tmp/backimage-protoc/bin/protoc; fi
if [ -z "$protoc_bin" ]; then protoc_bin=$(command -v protoc || true); fi

plugin_bin=${PROTOC_GEN_GO:-}
if [ -z "$plugin_bin" ] && [ -x /tmp/backimage-protoc/bin/protoc-gen-go ]; then plugin_bin=/tmp/backimage-protoc/bin/protoc-gen-go; fi
if [ -z "$plugin_bin" ] && [ -n "${GOBIN:-}" ] && [ -x "${GOBIN}/protoc-gen-go" ]; then plugin_bin="${GOBIN}/protoc-gen-go"; fi
if [ -z "$plugin_bin" ]; then plugin_bin=$(command -v protoc-gen-go || true); fi
if [ -z "$plugin_bin" ] && [ -x "$gopath_bin/protoc-gen-go" ]; then plugin_bin="$gopath_bin/protoc-gen-go"; fi

missing=""
[ -z "$protoc_bin" ] && missing="protoc"
[ -z "$plugin_bin" ] && missing="${missing:+$missing and }protoc-gen-go"

if [ -n "$missing" ]; then
	if [ "${BACKIMAGE_REQUIRE_PROTOC:-0}" = "1" ]; then
		echo "proto-check: $missing not found and BACKIMAGE_REQUIRE_PROTOC=1" >&2
		exit 1
	fi
	echo "SKIP: $missing not found, protobuf drift check not executed" >&2
	echo "      (install protoc 27.3 and protoc-gen-go v1.34.2, or set PROTOC/PROTOC_GEN_GO;" >&2
	echo "       set BACKIMAGE_REQUIRE_PROTOC=1 to turn this skip into a failure)" >&2
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/pkg/protocol" "$tmp/bin"
cp pkg/protocol/backimage.proto "$tmp/pkg/protocol/backimage.proto"
ln -s "$plugin_bin" "$tmp/bin/protoc-gen-go"
PATH="$tmp/bin:$PATH" "$protoc_bin" -I "$tmp" --go_out="$tmp" --go_opt=paths=source_relative "$tmp/pkg/protocol/backimage.proto"
cmp pkg/protocol/backimage.pb.go "$tmp/pkg/protocol/backimage.pb.go"
echo "protobuf generated sources are current"
