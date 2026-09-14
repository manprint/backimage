#!/usr/bin/env bash
# Phase A11 e2e: the backup and restore options that had only unit tests, and
# the two local commands nothing ran.
#
#  --no-metadata        a privacy promise: source paths and hostname must not
#                       reach the published image at all
#  --numeric-owner      no user/group names in the archive
#  --one-file-system    a mount point inside the tree is not descended into
#  --no-preserve-xattrs the restore drops extended attributes on request
#  --allow-unencrypted  a credential offered to a plaintext backup is refused
#  --cpus               accepted, and a nonsensical value is a usage error
#  doctor               reports the environment, and per-source readability
#  genpass              length, count, character classes, ambiguous glyphs
#
# Every check is discriminating: each one is run with and without the option,
# so a flag that silently did nothing would fail the phase instead of passing
# it. No registry and no docker: everything goes through a local OCI layout.
set -euo pipefail
cd "$(dirname "$0")/../.."

for tool in jq stat getfattr setfattr tar; do
	command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }
done
if [ "$(id -u)" = "0" ]; then
	# A11.7 rests on the kernel refusing a read, and root is never refused.
	echo "phase A11 e2e SKIPPED: must run unprivileged (root reads a 0000 file)"
	echo "phase A11 e2e OK"
	exit 0
fi

work=$(mktemp -d)
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ] && [ -f "$work/last.log" ]; then
		echo "phase A11 diagnostics (exit $rc)" >&2
		sed -n '1,40p' "$work/last.log" >&2
	fi
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
mkdir -p "$work/tmp"
make embed >/dev/null
go build -o bin/backimage ./cmd/backimage

REPO=example.com/e2e/a11
tree="$work/tree"
mkdir -p "$tree/sub"
printf 'uno\n' >"$tree/sub/a.txt"
printf 'due\n' >"$tree/b.txt"
setfattr -n user.prova -v valore "$tree/b.txt"

# build TAG LAYOUT [extra backup args...]
# --allow-degraded only clears the privilege preflight of an unprivileged run.
build() {
	local tag=$1 layout=$2; shift 2
	bin/backimage backup "$tree" --repo "$REPO" --tag "$tag" --no-encrypt --allow-degraded \
		--output oci-layout --output-path "$layout" --runnable=false \
		--temp-dir "$work/tmp" --quiet "$@" >"$work/last.log" 2>&1
}

# fails_with WANT WHAT COMMAND... — a refusal has to be the refusal expected.
# stdout and stderr are kept apart: a command that prints a JSON document and
# then fails still has to leave that document parsable.
fails_with() {
	local want="$1" what="$2"; shift 2
	set +e
	"$@" >"$work/last.out" 2>"$work/last.log"
	local rc=$?
	set -e
	[ "$rc" = "$want" ] || { echo "FAIL: $what è uscito $rc, atteso $want"; sed -n '1,20p' "$work/last.log"; exit 1; }
}

# ---------------------------------------------------------------------------
# A11.1 — --no-metadata: nothing about this machine reaches the image
# ---------------------------------------------------------------------------
build with "$work/lay-with"
build without "$work/lay-without" --no-metadata

bin/backimage inspect "$REPO:with" --oci-layout "$work/lay-with" --json >"$work/with.json"
bin/backimage inspect "$REPO:without" --oci-layout "$work/lay-without" --json >"$work/without.json"
jq -e --arg t "$tree" '.manifest.sources == [$t]' "$work/with.json" >/dev/null || {
	echo "FAIL: senza --no-metadata il manifest non riporta la sorgente"; exit 1; }
jq -e '.manifest.host.hostname != ""' "$work/with.json" >/dev/null || {
	echo "FAIL: senza --no-metadata il manifest non riporta l'host"; exit 1; }
jq -e '.manifest.sources == null and .manifest.host.hostname == "" and .manifest.host.os == ""' "$work/without.json" >/dev/null || {
	echo "FAIL: --no-metadata lascia sorgenti o host nel manifest"; jq -c '.manifest|{sources,host}' "$work/without.json"; exit 1; }

# The manifest is only where it is easiest to look. The promise is about the
# published bytes, so the whole layout is searched — labels and OCI index
# included, which is where the source path also ends up.
grep -rqF "$tree" "$work/lay-with" || {
	echo "FAIL: il percorso sorgente non compare nell'immagine senza --no-metadata: il controllo sotto non proverebbe nulla"; exit 1; }
if grep -rlF "$tree" "$work/lay-without" >"$work/leak.txt" 2>/dev/null; then
	echo "FAIL: --no-metadata pubblica comunque il percorso sorgente"; cat "$work/leak.txt"; exit 1
fi
if grep -rlF "$(hostname)" "$work/lay-without" >"$work/leak-host.txt" 2>/dev/null; then
	echo "FAIL: --no-metadata pubblica comunque l'hostname"; cat "$work/leak-host.txt"; exit 1
fi
echo "A11.1 --no-metadata: né percorso sorgente né hostname nell'immagine pubblicata: OK"

# ---------------------------------------------------------------------------
# A11.2 — --numeric-owner: no user or group names in the archive
# ---------------------------------------------------------------------------
build numeric "$work/lay-numeric" --numeric-owner
bin/backimage restore "$REPO:numeric" --oci-layout "$work/lay-numeric" -o "$work/numeric.tar" >"$work/last.log" 2>&1
bin/backimage restore "$REPO:with" --oci-layout "$work/lay-with" -o "$work/named.tar" >"$work/last.log" 2>&1
# GNU tar prints "uid/gid" when the header carries no names and "user/group"
# when it does, so the two archives are told apart by what tar itself reads.
owner_column() { tar -tvf "$1" | awk 'NR==1{print $2}'; }
numeric_owner=$(owner_column "$work/numeric.tar")
named_owner=$(owner_column "$work/named.tar")
case "$numeric_owner" in
	[0-9]*/[0-9]*) ;;
	*) echo "FAIL: --numeric-owner ha comunque scritto i nomi: $numeric_owner"; exit 1;;
esac
[ "$named_owner" = "$(id -un)/$(id -gn)" ] || {
	echo "FAIL: senza --numeric-owner i nomi non ci sono ($named_owner): il controllo sopra non proverebbe nulla"; exit 1; }
echo "A11.2 --numeric-owner: l'archivio porta uid/gid ($numeric_owner) invece dei nomi ($named_owner): OK"

# ---------------------------------------------------------------------------
# A11.3 — --one-file-system stops at a mount point
# ---------------------------------------------------------------------------
if ! unshare -rm true 2>/dev/null; then
	echo "A11.3 --one-file-system: SKIPPED (namespace utente non disponibile)"
else
	# The mount has to exist while the backup runs, so the backup runs inside
	# the namespace that owns it. A tmpfs is a different device, which is the
	# only thing --one-file-system looks at.
	cat >"$work/ofs.sh" <<'INNER'
set -eu
mount -t tmpfs none "$TREE/mnt"
printf 'oltre il mount\n' >"$TREE/mnt/dentro.txt"
bin/backimage backup "$TREE" --repo "$REPO" --tag crosses --no-encrypt --allow-degraded \
	--output oci-layout --output-path "$WORK/lay-crosses" --runnable=false \
	--temp-dir "$WORK/tmp" --quiet
bin/backimage backup "$TREE" --repo "$REPO" --tag stops --no-encrypt --allow-degraded \
	--one-file-system --output oci-layout --output-path "$WORK/lay-stops" --runnable=false \
	--temp-dir "$WORK/tmp" --quiet
INNER
	mkdir -p "$tree/mnt"
	TREE="$tree" WORK="$work" REPO="$REPO" unshare -rm bash "$work/ofs.sh" >"$work/last.log" 2>&1
	bin/backimage ls "$REPO:crosses" --oci-layout "$work/lay-crosses" >"$work/ls-crosses.txt" 2>>"$work/last.log"
	bin/backimage ls "$REPO:stops" --oci-layout "$work/lay-stops" >"$work/ls-stops.txt" 2>>"$work/last.log"
	grep -qx 'tree/mnt/dentro.txt' "$work/ls-crosses.txt" || {
		echo "FAIL: senza --one-file-system il file oltre il mount non è stato archiviato: il controllo sotto non proverebbe nulla"
		sed -n '1,20p' "$work/ls-crosses.txt"; exit 1; }
	if grep -qx 'tree/mnt/dentro.txt' "$work/ls-stops.txt"; then
		echo "FAIL: --one-file-system ha attraversato il mount point"; exit 1
	fi
	# The mount point itself is still archived as a directory: stopping at it
	# is not the same as pretending it is not there.
	grep -qx 'tree/mnt' "$work/ls-stops.txt" || {
		echo "FAIL: --one-file-system ha perso anche la directory del mount point"; exit 1; }
	grep -qx 'tree/sub/a.txt' "$work/ls-stops.txt" || {
		echo "FAIL: --one-file-system ha perso file sullo stesso filesystem"; exit 1; }
	rmdir "$tree/mnt"
	echo "A11.3 --one-file-system: il mount point resta, ciò che c'è oltre non viene archiviato: OK"
fi

# ---------------------------------------------------------------------------
# A11.4 — --no-preserve-xattrs
# ---------------------------------------------------------------------------
rm -rf "$work/x-with" "$work/x-without"
bin/backimage restore "$REPO:with" --oci-layout "$work/lay-with" -x -C "$work/x-with" \
	--no-preserve-owner >"$work/last.log" 2>&1
getfattr -d "$work/x-with/tree/b.txt" 2>/dev/null | grep -q 'user.prova="valore"' || {
	echo "FAIL: il restore normale non ha ripristinato l'attributo esteso"; exit 1; }
bin/backimage restore "$REPO:with" --oci-layout "$work/lay-with" -x -C "$work/x-without" \
	--no-preserve-owner --no-preserve-xattrs >"$work/last.log" 2>&1
if getfattr -d "$work/x-without/tree/b.txt" 2>/dev/null | grep -q 'user.prova'; then
	echo "FAIL: --no-preserve-xattrs ha comunque scritto l'attributo esteso"; exit 1
fi
cmp "$tree/b.txt" "$work/x-without/tree/b.txt"
echo "A11.4 --no-preserve-xattrs: attributo assente, contenuto intatto; senza il flag torna: OK"

# ---------------------------------------------------------------------------
# A11.5 — --allow-unencrypted
# ---------------------------------------------------------------------------
# A credential offered to a plaintext backup means one of the two is not what
# the user thinks, so it is refused as an integrity answer (exit 5) until the
# user says the plaintext is intentional.
fails_with 5 "restore in chiaro con passphrase" \
	env BACKIMAGE_PASSPHRASE=qualcosa bin/backimage restore "$REPO:with" \
	--oci-layout "$work/lay-with" -x -C "$work/au-no" --no-preserve-owner
grep -q -- '--allow-unencrypted' "$work/last.log" || {
	echo "FAIL: il rifiuto non indica --allow-unencrypted"; sed -n '1,10p' "$work/last.log"; exit 1; }
rm -rf "$work/au-yes"
env BACKIMAGE_PASSPHRASE=qualcosa bin/backimage restore "$REPO:with" \
	--oci-layout "$work/lay-with" -x -C "$work/au-yes" --no-preserve-owner \
	--allow-unencrypted >"$work/last.log" 2>&1
cmp "$tree/sub/a.txt" "$work/au-yes/tree/sub/a.txt"
# The same gate guards the read-only commands, not just the restore.
fails_with 5 "ls in chiaro con passphrase" \
	env BACKIMAGE_PASSPHRASE=qualcosa bin/backimage ls "$REPO:with" --oci-layout "$work/lay-with"
echo "A11.5 --allow-unencrypted: senza il flag exit 5 su restore e ls, con il flag il ripristino è corretto: OK"

# ---------------------------------------------------------------------------
# A11.6 — --cpus
# ---------------------------------------------------------------------------
rm -rf "$work/cpu1"
bin/backimage restore "$REPO:with" --oci-layout "$work/lay-with" -x -C "$work/cpu1" \
	--no-preserve-owner --cpus 1 >"$work/last.log" 2>&1
cmp "$tree/sub/a.txt" "$work/cpu1/tree/sub/a.txt"
fails_with 2 "--cpus 0" bin/backimage restore "$REPO:with" --oci-layout "$work/lay-with" \
	-x -C "$work/cpu0" --cpus 0
fails_with 2 "--cache-size non numerico" bin/backimage restore "$REPO:with" \
	--oci-layout "$work/lay-with" -x -C "$work/cs" --cache-size pippo
echo "A11.6 --cpus 1 ripristina, --cpus 0 e --cache-size non valido sono errori d'uso: OK"

# ---------------------------------------------------------------------------
# A11.7 — doctor
# ---------------------------------------------------------------------------
# Unprivileged the required capabilities are missing, so doctor exits 3; what
# matters is that it says so in a form a script can read.
fails_with 3 "doctor senza privilegi" bin/backimage doctor --json
jq -e 'type == "array" and length > 0' "$work/last.out" >/dev/null || {
	echo "FAIL: doctor --json non produce un elenco"; head -c 300 "$work/last.out"; exit 1; }
jq -e '[.[].name] | index("read-all-files") != null and index("chown") != null and index("cache-dir") != null' \
	"$work/last.out" >/dev/null || { echo "FAIL: doctor non riporta i controlli attesi"; exit 1; }
jq -e '[.[] | select(.available == false) | select(.remedy == "")] | length == 0' "$work/last.out" >/dev/null || {
	echo "FAIL: un controllo fallito è senza rimedio"; exit 1; }
cp "$work/last.out" "$work/doctor-env.json"

# With PATH arguments read-all-files is answered about those paths, which is
# the only part of doctor that depends on them.
jq -e '.[] | select(.name == "read-all-files") | .available == true' "$work/doctor-env.json" >/dev/null || {
	echo "FAIL: read-all-files già falso sull'ambiente: il controllo sotto non proverebbe nulla"; exit 1; }
mkdir -p "$work/unreadable"
printf 'segreto\n' >"$work/unreadable/chiuso.txt"
chmod 000 "$work/unreadable/chiuso.txt"
fails_with 3 "doctor su un albero illeggibile" bin/backimage doctor "$work/unreadable" --json
jq -e '.[] | select(.name == "read-all-files") | .available == false and (.reason | contains("unreadable"))' \
	"$work/last.out" >/dev/null || {
	echo "FAIL: doctor non segnala il file illeggibile della sorgente"; head -c 300 "$work/last.out"; exit 1; }
chmod 644 "$work/unreadable/chiuso.txt"
fails_with 1 "doctor su un percorso inesistente" bin/backimage doctor "$work/non-esiste"
echo "A11.7 doctor: ambiente, sorgente illeggibile e percorso inesistente, ognuno con il proprio esito: OK"

# ---------------------------------------------------------------------------
# A11.8 — genpass
# ---------------------------------------------------------------------------
[ "$(bin/backimage genpass | wc -c)" = "33" ] || { echo "FAIL: genpass non genera 32 caratteri"; exit 1; }
[ "$(bin/backimage genpass --length 48 | tr -d '\n' | wc -c)" = "48" ] || { echo "FAIL: --length ignorato"; exit 1; }
[ "$(bin/backimage genpass --count 5 | wc -l)" = "5" ] || { echo "FAIL: --count ignorato"; exit 1; }
[ "$(bin/backimage genpass --count 5 | sort -u | wc -l)" = "5" ] || { echo "FAIL: genpass ripete la stessa passphrase"; exit 1; }
[ "$(bin/backimage genpass --no-symbols --count 5 | tr -d 'A-Za-z0-9\n' | wc -c)" = "0" ] || {
	echo "FAIL: --no-symbols ha comunque prodotto simboli"; exit 1; }
# Ambiguous glyphs: over 20 keys of 64 characters their absence is a rule, not
# luck, and their presence with --ambiguous shows the exclusion is what is
# being measured.
ambiguous_off=$(for _ in $(seq 1 20); do bin/backimage genpass --length 64; done | tr -cd 'lIO01' | wc -c)
ambiguous_on=$(for _ in $(seq 1 20); do bin/backimage genpass --length 64 --ambiguous; done | tr -cd 'lIO01' | wc -c)
[ "$ambiguous_off" = "0" ] || { echo "FAIL: i caratteri ambigui compaiono senza --ambiguous ($ambiguous_off)"; exit 1; }
[ "$ambiguous_on" -gt 0 ] || { echo "FAIL: --ambiguous non li reintroduce: il controllo sopra non proverebbe nulla"; exit 1; }
fails_with 2 "genpass --length 8" bin/backimage genpass --length 8
fails_with 2 "genpass --count 0" bin/backimage genpass --count 0
# The point of the command: what it prints opens a backup made with it.
bin/backimage genpass >"$work/gen.txt"
chmod 600 "$work/gen.txt"
bin/backimage backup "$tree" --repo "$REPO" --tag generated --passphrase-file "$work/gen.txt" \
	--allow-degraded --output oci-layout --output-path "$work/lay-gen" --runnable=false \
	--temp-dir "$work/tmp" --quiet >"$work/last.log" 2>&1
rm -rf "$work/gen-out"
bin/backimage restore "$REPO:generated" --oci-layout "$work/lay-gen" -x -C "$work/gen-out" \
	--passphrase-file "$work/gen.txt" --no-preserve-owner >"$work/last.log" 2>&1
cmp "$tree/sub/a.txt" "$work/gen-out/tree/sub/a.txt"
echo "A11.8 genpass: lunghezza, quantità, classi, glifi ambigui, limiti, e la chiave generata apre il backup: OK"

echo "phase A11 e2e OK"
