#!/usr/bin/env bash
# Phase 04 e2e: push a two-platform image to a local registry, pull it with
# docker, inspect entrypoint/labels and prove that the shared data layers are
# uploaded exactly once (GS-04.3, GS-04.4, GS-04.5).
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase 04 e2e SKIPPED: docker not available"
	echo "phase 04 e2e OK"
	exit 0
fi
for tool in go jq curl; do
	command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }
done

PORT=${PHASE04_PORT:-5000}
NAME=bi-registry-p04
REG="localhost:${PORT}/test/img:v1"
# A registry of its own, removed on exit with the image the phase pulls: a
# registry left running from an earlier run may listen on another port, or
# already hold the tag this phase is meant to publish.
cleanup() {
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	docker image rm "$REG" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p "${PORT}:5000" registry:2 >/dev/null
ready=0
for _ in $(seq 1 100); do
	if curl -fsS "http://localhost:${PORT}/v2/" >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 0.05
done
[ "$ready" = 1 ] || { echo "registry on port ${PORT} did not start"; exit 1; }

echo "==> push"
go run ./test/e2e/tools/publishimg "${REG}"

echo "==> docker pull amd64"
docker pull --platform linux/amd64 "${REG}" >/dev/null

echo "==> docker inspect: entrypoint and labels"
docker inspect "$REG" | jq -e '.[0].Config.Entrypoint[0] == "/backimage"' >/dev/null
docker inspect "$REG" | jq -e '.[0].Config.Cmd[0] == "info"' >/dev/null
docker inspect "$REG" | jq -e '.[0].Config.Labels["dev.backimage.schema-version"] == "1"' >/dev/null
docker inspect "$REG" | jq -e '.[0].Config.Labels["dev.backimage.sources"] == "/fake/data"' >/dev/null
echo "inspect ok"

echo "==> manifest list via v2 API: two platforms"
m=$(curl -sf -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json" \
	"http://localhost:${PORT}/v2/test/img/manifests/v1")
n=$(echo "$m" | jq -r '.manifests | length')
[ "$n" = "2" ] || { echo "expected 2 manifests, got $n"; exit 1; }

echo "==> blob deduplication: 5 unique layer blobs (1 exe + 1 meta + 3 data)"
layers=()
for arch in amd64 arm64; do
	dg=$(echo "$m" | jq -r ".manifests[] | select(.platform.architecture == \"$arch\") | .digest")
	mapfile -t ls < <(curl -sf -H "Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json" \
		"http://localhost:${PORT}/v2/test/img/manifests/${dg}" | jq -r '.layers[].digest')
	layers+=("${ls[@]}")
done
unique=$(printf '%s\n' "${layers[@]}" | sort -u | wc -l)
total=$(printf '%s\n' "${layers[@]}" | wc -l)
[ "$unique" = "5" ] || { echo "expected 5 unique layer blobs, got $unique (total $total)"; exit 1; }
dups=$(printf '%s\n' "${layers[@]}" | sort | uniq -c | grep -cE '^ *2 ' || true)
[ "$dups" = "5" ] || { echo "expected 5 twice-referenced layers, got $dups"; exit 1; }
echo "blobs ok: $unique unique / $total references (3 data layers shared between platforms)"

echo "phase 04 e2e OK"
