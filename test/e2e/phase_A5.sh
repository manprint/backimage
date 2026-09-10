#!/usr/bin/env bash
# Phase A5 e2e: an image whose entrypoint was replaced is refused before the
# passphrase is read, the confined profile restores ordinary files without
# any privilege, and full fidelity is a profile that has to be asked for.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
	echo "phase A5 e2e SKIPPED: docker not available"
	echo "phase A5 e2e OK"
	exit 0
fi
for tool in curl jq timeout; do command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }; done

PORT=${PHASEA5_REGISTRY_PORT:-5051}
NAME=bi-registry-pA5
HOST="localhost:${PORT}"
REPO="${HOST}/e2e/a5"
IMAGE="${REPO}:t1"
work=$(mktemp -d)
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase A5 diagnostics (exit $rc)" >&2
		for log in anchored.err control.err confined.log owner.log strict.err fidelity.log; do
			if [ -f "$work/$log" ]; then echo "[$log]" >&2; sed -n '1,60p' "$work/$log" >&2; fi
		done
	fi
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	docker rmi -f "$IMAGE" >/dev/null 2>&1 || true
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work" 2>/dev/null || { command -v sudo >/dev/null 2>&1 && sudo -n rm -rf "$work" 2>/dev/null; } || true
	return "$rc"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
tree="$work/tree"
mkdir -p "$tree/sub" "$work/tmp"
printf 'phase A5 payload\n' >"$tree/sub/a.txt"
printf 'second file\n' >"$tree/b.txt"
ln -s sub/a.txt "$tree/link-to-a"
chmod 0640 "$tree/b.txt"
secret='phaseA5-e2e-passphrase'
printf '%s\n' "$secret" >"$work/pass"
chmod 600 "$work/pass"

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p "${PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 100); do curl -fsS "http://${HOST}/v2/" >/dev/null && break; sleep 0.05; done

echo "==> honest backup, and the digest it reports"
make embed >/dev/null
bin/backimage backup "$tree" --repo "$REPO" --tag t1 \
	--passphrase-file "$work/pass" --allow-degraded \
	--platform linux/amd64 --temp-dir "$work/tmp" --json >"$work/backup.json"
DIGEST=$(jq -r .digest <"$work/backup.json")
case "$DIGEST" in
sha256:*) ;;
*) echo "FAIL: backup reported no digest: $(cat "$work/backup.json")"; exit 1 ;;
esac
docker pull --platform linux/amd64 "$IMAGE" >/dev/null

echo "==> the reported digest anchors the honest image"
bin/backimage restore "$IMAGE" --expect-digest "$DIGEST" \
	-x -C "$work/anchored" --passphrase-file "$work/pass" >/dev/null
diff -r "$tree" "$work/anchored/$(basename "$tree")" >/dev/null

echo "==> confined profile: ordinary files restore with no privilege at all"
mkdir -p "$work/confined"
BACKIMAGE_PASSPHRASE="$secret" docker run --rm \
	--network none \
	--read-only --tmpfs /tmp \
	--cap-drop ALL --security-opt no-new-privileges \
	--user "$(id -u):$(id -g)" \
	-e BACKIMAGE_PASSPHRASE \
	-v "$work/confined:/restore" \
	"$IMAGE" extract --out /restore --no-preserve-owner >"$work/confined.log" 2>&1
grep -q 'esito 1:1 sulle entry ricevute' "$work/confined.log"
diff -r "$tree" "$work/confined/$(basename "$tree")" >/dev/null
test -L "$work/confined/$(basename "$tree")/link-to-a"
test "$(stat -c %a "$work/confined/$(basename "$tree")/b.txt")" = 640

echo "==> ownership is a declared profile, not something the default profile fakes"
mkdir -p "$work/owner"
chmod 0777 "$work/owner"
set +e
BACKIMAGE_PASSPHRASE="$secret" docker run --rm \
	--network none --read-only --tmpfs /tmp \
	--cap-drop ALL --security-opt no-new-privileges \
	--user 65534:65534 \
	-e BACKIMAGE_PASSPHRASE \
	-v "$work/owner:/restore" \
	"$IMAGE" extract --out /restore >"$work/owner.log" 2>&1
owner_rc=$?
set -e
[ "$owner_rc" -eq 0 ] || { echo "FAIL: a degraded restore must still succeed (exit $owner_rc)"; exit 1; }
grep -q 'esito NON 1:1' "$work/owner.log" || { echo "FAIL: the summary must declare the degradation"; exit 1; }
grep -q 'differenza owner' "$work/owner.log" || { echo "FAIL: the degraded class must be named"; exit 1; }
test -s "$work/owner/$(basename "$tree")/b.txt"

echo "==> and --strict refuses to pretend, naming the way out"
rm -rf "$work/owner-strict"; mkdir -p "$work/owner-strict"; chmod 0777 "$work/owner-strict"
set +e
BACKIMAGE_PASSPHRASE="$secret" docker run --rm \
	--network none --read-only --tmpfs /tmp \
	--cap-drop ALL --security-opt no-new-privileges \
	--user 65534:65534 \
	-e BACKIMAGE_PASSPHRASE \
	-v "$work/owner-strict:/restore" \
	"$IMAGE" extract --out /restore --strict >"$work/strict.err" 2>&1
strict_rc=$?
set -e
[ "$strict_rc" -ne 0 ] || { echo "FAIL: --strict must fail when ownership cannot be applied"; exit 1; }
grep -q -- '--no-preserve-owner' "$work/strict.err" || { echo "FAIL: the refusal must name the remediation"; exit 1; }

echo "==> full fidelity gate: ownership, xattr and device"
if sudo -n true >/dev/null 2>&1 && command -v setfattr >/dev/null 2>&1 && command -v getfattr >/dev/null 2>&1; then
	fid="$work/fidelity"
	sudo -n mkdir -p "$fid/src"
	sudo -n sh -c "printf 'owned by root\n' >'$fid/src/root.txt'"
	sudo -n chown 0:0 "$fid/src/root.txt"
	sudo -n mknod "$fid/src/nulldev" c 1 3
	sudo -n setfattr -n trusted.e2e -v phaseA5 "$fid/src/root.txt"
	sudo -n env XDG_CONFIG_HOME="$XDG_CONFIG_HOME" XDG_CACHE_HOME="$XDG_CACHE_HOME" \
		BACKIMAGE_AUTH_FILE="$BACKIMAGE_AUTH_FILE" \
		bin/backimage backup "$fid/src" --repo "$REPO" --tag fid \
		--passphrase-file "$work/pass" --platform linux/amd64 \
		--temp-dir "$work/tmp" >/dev/null
	docker pull --platform linux/amd64 "${REPO}:fid" >/dev/null
	sudo -n mkdir -p "$fid/out"
	sudo -n env BACKIMAGE_PASSPHRASE="$secret" docker run --rm --privileged \
		-e BACKIMAGE_PASSPHRASE -v "$fid/out:/restore" \
		"${REPO}:fid" extract --out /restore --strict >"$work/fidelity.log" 2>&1
	grep -q 'esito 1:1 sulle entry ricevute' "$work/fidelity.log" \
		|| { echo "FAIL: the declared full-fidelity profile must be 1:1"; exit 1; }
	sudo -n test -c "$fid/out/src/nulldev" || { echo "FAIL: device node not restored"; exit 1; }
	[ "$(sudo -n stat -c %u "$fid/out/src/root.txt")" = 0 ] || { echo "FAIL: ownership not restored"; exit 1; }
	sudo -n getfattr -n trusted.e2e --only-values "$fid/out/src/root.txt" 2>/dev/null | grep -q phaseA5 \
		|| { echo "FAIL: trusted.* xattr not restored"; exit 1; }
	docker rmi -f "${REPO}:fid" >/dev/null 2>&1 || true
else
	echo "    SKIPPED: needs passwordless sudo and setfattr to build a fixture with a device node,"
	echo "    a foreign owner and a trusted.* xattr. The confined gates above still ran."
fi

echo "==> a substituted entrypoint takes the passphrase, and nothing inside the image can stop it"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$work/leakpass" ./test/e2e/tools/leakpass
cat >"$work/Dockerfile" <<DOCKERFILE
FROM ${IMAGE}
COPY leakpass /backimage
ENTRYPOINT ["/backimage"]
DOCKERFILE
docker build --platform linux/amd64 -t "$IMAGE" -f "$work/Dockerfile" "$work" >/dev/null
docker push "$IMAGE" >/dev/null
mkdir -p "$work/leak"
BACKIMAGE_PASSPHRASE="$secret" docker run --rm \
	--user "$(id -u):$(id -g)" -e BACKIMAGE_PASSPHRASE -e LEAKPASS_OUT=/restore \
	-v "$work/leak:/restore" "$IMAGE" extract --out /restore >/dev/null 2>&1
grep -qx "$secret" "$work/leak/leaked-passphrase" \
	|| { echo "FAIL: the substitution fixture did not reproduce the loss"; exit 1; }

echo "==> the host binary, anchored to the honest digest, refuses it without reading the secret"
mkfifo "$work/pass.fifo"
set +e
timeout 60 bin/backimage restore "$IMAGE" --expect-digest "$DIGEST" \
	-x -C "$work/refused" --passphrase-file "$work/pass.fifo" >/dev/null 2>"$work/anchored.err"
rc=$?
set -e
[ "$rc" -eq 5 ] || { echo "FAIL: expected exit 5 (integrity), got $rc"; exit 1; }
grep -q "$DIGEST" "$work/anchored.err" || { echo "FAIL: the refusal must name the expected digest"; exit 1; }
grep -q 'pass.fifo' "$work/anchored.err" && { echo "FAIL: the passphrase file was named, so it was reached"; exit 1; }
test ! -e "$work/refused" || { echo "FAIL: nothing must have been written"; exit 1; }

echo "==> control: without the anchor the same command does reach that passphrase"
set +e
timeout 10 bin/backimage restore "$IMAGE" \
	-x -C "$work/control" --passphrase-file "$work/pass.fifo" >/dev/null 2>"$work/control.err"
control_rc=$?
set -e
# The fifo has no writer: reading it blocks forever, so a timeout is the proof
# that the read happened. Anything else means the control is not measuring
# what the anchored run avoided.
[ "$control_rc" -eq 124 ] || {
	echo "FAIL: the control run must block on the passphrase fifo, got exit $control_rc"
	exit 1
}

echo "==> no e2e script mounts the Docker socket"
if grep -rn 'docker\.sock' test/e2e/*.sh; then
	echo "FAIL: an e2e script mounts the daemon socket"
	exit 1
fi

echo "phase A5 e2e OK"
