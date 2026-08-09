#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

protoc_bin=${PROTOC:-}
if [ -z "$protoc_bin" ] && [ -x /tmp/backimage-protoc/bin/protoc ]; then protoc_bin=/tmp/backimage-protoc/bin/protoc; fi
if [ -z "$protoc_bin" ]; then protoc_bin=$(command -v protoc || true); fi
plugin_bin=${PROTOC_GEN_GO:-}
if [ -z "$plugin_bin" ] && [ -x /tmp/backimage-protoc/bin/protoc-gen-go ]; then plugin_bin=/tmp/backimage-protoc/bin/protoc-gen-go; fi
if [ -z "$plugin_bin" ] && [ -x "${GOBIN:-}/protoc-gen-go" ]; then plugin_bin="${GOBIN}/protoc-gen-go"; fi
if [ -z "$plugin_bin" ]; then plugin_bin=$(command -v protoc-gen-go || true); fi
if [ -z "$protoc_bin" ] || [ -z "$plugin_bin" ]; then
	echo "proto-check requires protoc and protoc-gen-go (generated .pb.go remains buildable without them)" >&2
	exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/pkg/protocol" "$tmp/bin"
cp pkg/protocol/backimage.proto "$tmp/pkg/protocol/backimage.proto"
ln -s "$plugin_bin" "$tmp/bin/protoc-gen-go"
PATH="$tmp/bin:$PATH" "$protoc_bin" -I "$tmp" --go_out="$tmp" --go_opt=paths=source_relative "$tmp/pkg/protocol/backimage.proto"
cmp pkg/protocol/backimage.pb.go "$tmp/pkg/protocol/backimage.pb.go"
echo "protobuf generated sources are current"
