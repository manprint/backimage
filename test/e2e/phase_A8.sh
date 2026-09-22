#!/usr/bin/env bash
# Phase A8 e2e: the two invariants a backup tool is worth nothing without, and
# the three defects found while auditing them.
#
#  1. a backup never modifies the source tree;
#  2. --allow-degraded survives a file whose content cannot be read;
#  3. a strict run refuses that same tree instead of publishing a hole;
#  4. entries are emitted alphabetically at every depth, not just below the root;
#  5. --strict exits non-zero when the restore completed but was not 1:1.
#
# No registry and no docker: everything goes through the real CLI against a
# real encrypted backup in a local OCI layout, so the phase is fast and has no
# moving parts other than the code under test.
set -euo pipefail
cd "$(dirname "$0")/../.."

for tool in jq stat sha256sum getfattr setfacl mkfifo tar; do
	command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }
done
if [ "$(id -u)" = "0" ]; then
	# Every unreadable-file check below rests on the kernel refusing a read.
	# root is never refused, so the phase would assert nothing.
	echo "phase A8 e2e SKIPPED: must run unprivileged (root reads a 0000 file)"
		exit 0
fi

work=$(mktemp -d)
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "phase A8 diagnostics (exit $rc)" >&2
		for log in backup.log degraded.log strict.err restore.log fidelity.log; do
			if [ -f "$work/$log" ]; then echo "[$log]" >&2; sed -n '1,40p' "$work/$log" >&2; fi
		done
	fi
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
secret='e2e-A8-passphrase-lunga-abbastanza'
printf '%s\n' "$secret" >"$work/pass.txt"
mkdir -p "$work/tmp"

REF=example.com/e2e/a8:t1

# snapshot prints one line per path with every field a write would move.
# ctime is the decisive one: it changes on any metadata write, and no
# userspace call can set it back.
snapshot() {
	local root=$1
	(
		cd "$root"
		find . -print0 | sort -z | while IFS= read -r -d '' p; do
			local meta extra
			meta=$(stat -c '%F|%a|%u|%g|%s|%.9Y|%.9Z|%i|%h' -- "$p")
			extra=""
			if [ -L "$p" ]; then
				extra="link:$(readlink -- "$p")"
			elif [ -f "$p" ] && [ -r "$p" ]; then
				extra="sha:$(sha256sum -- "$p" | cut -d' ' -f1)"
			fi
			printf '%s|%s|%s|%s\n' "$p" "$meta" "$extra" \
				"$(getfattr -d -m '.*' -h -- "$p" 2>/dev/null | tail -n +2 | tr '\n' ';')"
		done
	)
}

# ---------------------------------------------------------------------------
# 1. the source is never modified
# ---------------------------------------------------------------------------
tree="$work/tree"
mkdir -p "$tree/sub/deep" "$tree/perms" "$tree/dir con spazi"
printf 'contenuto uno\n' >"$tree/sub/a.txt"
printf 'contenuto due\n' >"$tree/sub/deep/b.txt"
printf 'con spazi\n' >"$tree/dir con spazi/file.txt"
printf 'backslash\n' >"$tree/a\\b.txt"
head -c 200000 /dev/urandom >"$tree/big.bin"
ln -s sub/a.txt "$tree/link-rel"
ln -s nonesiste "$tree/link-rotto"
printf 'hardlink\n' >"$tree/perms/orig.txt"
ln "$tree/perms/orig.txt" "$tree/perms/link2.txt"
printf 'suid\n' >"$tree/perms/suid"
chmod 4755 "$tree/perms/suid"
printf 'attributi\n' >"$tree/xattr.txt"
setfattr -n user.prova -v 'valore' "$tree/xattr.txt"
touch -d '1999-12-31 23:59:58' "$tree/sub/deep/b.txt"

snapshot "$tree" >"$work/before.txt"
before_count=$(wc -l <"$work/before.txt")
[ "$before_count" -ge 12 ] || { echo "FAIL: fixture troppo piccola ($before_count path)"; exit 1; }

# --allow-degraded only to clear the privilege preflight, which an
# unprivileged run cannot satisfy (chown). The tree itself is entirely
# readable, so nothing is actually degraded: A8.4 asserts that separately.
bin/backimage backup "$tree" --repo example.com/e2e/a8 --tag t1 \
	--passphrase-file "$work/pass.txt" --output oci-layout --output-path "$work/layout" \
	--runnable=false --temp-dir "$work/tmp" --allow-degraded >"$work/backup.log" 2>&1

snapshot "$tree" >"$work/after.txt"
if ! diff -u "$work/before.txt" "$work/after.txt" >"$work/source-diff.txt"; then
	echo "FAIL: il backup ha modificato la sorgente"
	sed -n '1,40p' "$work/source-diff.txt"
	exit 1
fi
# Nothing new inside the source either: a spool or a lock file landing in the
# tree would not move any existing ctime, so the diff above would miss it.
[ "$(wc -l <"$work/after.txt")" = "$before_count" ] || { echo "FAIL: il backup ha creato oggetti nella sorgente"; exit 1; }
echo "A8.1 sorgente invariata dopo il backup ($before_count path, ctime compreso): OK"

# The access times too. A separate tree, because the snapshot above reads every
# file to hash it and so moves the atimes itself. The atimes are set older than
# the mtimes, which is when relatime — the default mount option — updates them
# on a read without O_NOATIME. Symlinks are left out: readlink updates the
# link's own atime and no flag avoids it (docs/FIDELITY.md).
atree="$work/atime-tree"
mkdir -p "$atree/sub"
printf 'letto dal backup\n' >"$atree/sub/file.txt"
head -c 70000 /dev/urandom >"$atree/big.bin"
touch -m -d '2020-01-01 00:00:00' "$atree/sub/file.txt" "$atree/big.bin" "$atree/sub" "$atree"
touch -a -d '2001-01-01 00:00:00' "$atree/sub/file.txt" "$atree/big.bin" "$atree/sub" "$atree"
# An explicit list, not find: listing a directory is a read that moves its
# atime, so find would update the directory atimes before measuring them.
atimes() { stat -c '%n|%.9X' -- "$atree" "$atree/sub" "$atree/sub/file.txt" "$atree/big.bin"; }
atimes >"$work/atime-before.txt"
bin/backimage backup "$atree" --repo example.com/e2e/a8 --tag atime \
	--passphrase-file "$work/pass.txt" --output oci-layout --output-path "$work/atime-layout" \
	--runnable=false --temp-dir "$work/tmp" --allow-degraded >"$work/backup.log" 2>&1
atimes >"$work/atime-after.txt"
if ! diff -u "$work/atime-before.txt" "$work/atime-after.txt"; then
	echo "FAIL: il backup ha aggiornato l'atime della sorgente"
	exit 1
fi
echo "A8.1b atime della sorgente invariati dopo il backup: OK"

# ---------------------------------------------------------------------------
# 2. emission order: alphabetical inside every directory, root included
# ---------------------------------------------------------------------------
bin/backimage restore "$REF" --oci-layout "$work/layout" -o "$work/order.tar" \
	--passphrase-file "$work/pass.txt" >>"$work/restore.log" 2>&1
tar -tf "$work/order.tar" | sed 's:/$::' >"$work/order.txt"
# Group by parent and require each group to be sorted. Before the fix the
# root's own children came out reverse-alphabetically.
python3 - "$work/order.txt" <<'PY'
import sys, os, collections
paths = [l.rstrip("\n") for l in open(sys.argv[1]) if l.strip()]
seen, groups = set(), collections.defaultdict(list)
for p in paths:
    parent = os.path.dirname(p)
    groups[parent].append(os.path.basename(p))
    if parent and parent not in seen:
        sys.exit(f"FAIL: {p} emesso prima della sua directory {parent}")
    seen.add(p)
for parent, names in groups.items():
    if names != sorted(names):
        sys.exit(f"FAIL: figli di {parent!r} emessi come {names}, attesi {sorted(names)}")
print(f"A8.2 ordine alfabetico a ogni livello ({len(paths)} entry, {len(groups)} directory): OK")
PY

# ---------------------------------------------------------------------------
# 3. a file whose content cannot be read
# ---------------------------------------------------------------------------
dtree="$work/degraded"
mkdir -p "$dtree"
printf 'leggibile\n' >"$dtree/leggibile.txt"
printf 'mai letto da nessuno\n' >"$dtree/segreto.txt"
chmod 000 "$dtree/segreto.txt"

# 3a. strict (the default) refuses the tree instead of publishing a hole.
set +e
bin/backimage backup "$dtree" --repo example.com/e2e/a8 --tag strict \
	--passphrase-file "$work/pass.txt" --output oci-layout --output-path "$work/layout-strict" \
	--runnable=false --temp-dir "$work/tmp" >"$work/strict.err" 2>&1
strict_rc=$?
set -e
[ "$strict_rc" -ne 0 ] || { echo "FAIL: un backup strict ha accettato un file illeggibile"; exit 1; }
grep -q 'read-all-files' "$work/strict.err" || { echo "FAIL: il rifiuto non nomina la capability mancante"; sed -n '1,20p' "$work/strict.err"; exit 1; }
echo "A8.3 backup strict rifiuta il file illeggibile (exit $strict_rc, nomina read-all-files): OK"

# 3b. --allow-degraded completes. This is the regression: the entry of an
# unreadable file used to be emitted without a SHA256, the index schema
# refuses that, and the run died with "entry[N] bad sha256" *after* archiving,
# compressing and encrypting everything.
bin/backimage --json backup "$dtree" --repo example.com/e2e/a8 --tag degraded \
	--passphrase-file "$work/pass.txt" --output oci-layout --output-path "$work/layout-deg" \
	--runnable=false --temp-dir "$work/tmp" --allow-degraded >"$work/degraded.json" 2>"$work/degraded.log"

grep -q 'bad sha256' "$work/degraded.log" && { echo "FAIL: regressione, il difetto 'bad sha256' è tornato"; exit 1; }
skipped=$(jq -r '.contentSkipped' "$work/degraded.json")
[ "$skipped" = "1" ] || { echo "FAIL: contentSkipped = $skipped, atteso 1"; cat "$work/degraded.json"; exit 1; }
files=$(jq -r '.files' "$work/degraded.json")
[ "$files" = "2" ] || { echo "FAIL: files = $files, atteso 2 (il file illeggibile è archiviato, non scartato)"; exit 1; }
grep -q 'SENZA contenuto' "$work/degraded.log" || { echo "FAIL: nessun avviso sul contenuto perso"; sed -n '1,20p' "$work/degraded.log"; exit 1; }
echo "A8.4 --allow-degraded completa e dichiara contentSkipped=1: OK"

# 3c. the restore materialises it as an empty file, keeping its metadata.
bin/backimage restore example.com/e2e/a8:degraded --oci-layout "$work/layout-deg" \
	--extract -C "$work/deg-out" --passphrase-file "$work/pass.txt" \
	--no-preserve-owner >>"$work/restore.log" 2>&1
restored="$work/deg-out/$(basename "$dtree")/segreto.txt"
[ -f "$restored" ] || { echo "FAIL: il file illeggibile non è stato ripristinato"; exit 1; }
[ "$(stat -c %s "$restored")" = "0" ] || { echo "FAIL: il file illeggibile non è vuoto"; exit 1; }
[ "$(stat -c %a "$restored")" = "0" ] || { echo "FAIL: i permessi del file illeggibile non sono stati conservati"; exit 1; }
[ "$(cat "$work/deg-out/$(basename "$dtree")/leggibile.txt")" = "leggibile" ] || { echo "FAIL: il file leggibile è stato alterato"; exit 1; }
echo "A8.5 restore: file illeggibile ricreato vuoto con i suoi metadati, gli altri intatti: OK"

# ---------------------------------------------------------------------------
# 4. --strict promises a faithful copy, and the exit code has to keep it
# ---------------------------------------------------------------------------
# A fifo carrying a POSIX ACL is the one metadata loss a wholly unprivileged
# restore hits on any filesystem: there is no *at form of setxattr, so the
# object would have to be opened by name to receive its attributes, and a fifo
# cannot be opened without blocking. The attribute is dropped, counted, and
# reported — and with --strict that has to mean a non-zero exit.
ftree="$work/fifo"
mkdir -p "$ftree"
printf 'dati\n' >"$ftree/a.txt"
mkfifo "$ftree/pipe"
setfacl -m u:nobody:rwx "$ftree/pipe"

bin/backimage backup "$ftree" --repo example.com/e2e/a8 --tag fifo \
	--passphrase-file "$work/pass.txt" --output oci-layout --output-path "$work/layout-fifo" \
	--runnable=false --temp-dir "$work/tmp" --allow-degraded >>"$work/backup.log" 2>&1

# 4a. without --strict the restore succeeds and the difference is in --json.
bin/backimage --json restore example.com/e2e/a8:fifo --oci-layout "$work/layout-fifo" \
	--extract -C "$work/fifo-lax" --passphrase-file "$work/pass.txt" \
	--no-preserve-owner >"$work/fifo-lax.json" 2>>"$work/fidelity.log"
[ "$(jq -r '.ok' "$work/fifo-lax.json")" = "true" ] || { echo "FAIL: un restore non-strict degradato deve riuscire"; exit 1; }
[ "$(jq -r '.degraded["xattr.unopenable"]' "$work/fifo-lax.json")" = "1" ] || { echo "FAIL: la differenza non è in --json"; cat "$work/fifo-lax.json"; exit 1; }
[ "$(jq -r '.xattrs_skipped' "$work/fifo-lax.json")" = "1" ] || { echo "FAIL: xattrs_skipped assente da --json"; exit 1; }
jq -e '.degraded_examples["xattr.unopenable"] | test("pipe")' "$work/fifo-lax.json" >/dev/null || { echo "FAIL: --json non mostra quale oggetto ha perso l'attributo"; cat "$work/fifo-lax.json"; exit 1; }
echo "A8.6 restore degradato: esce 0 e riporta la differenza in --json: OK"

# 4b. with --strict the same restore completes and exits 8.
set +e
bin/backimage restore example.com/e2e/a8:fifo --oci-layout "$work/layout-fifo" \
	--extract -C "$work/fifo-strict" --passphrase-file "$work/pass.txt" \
	--no-preserve-owner --strict >>"$work/fidelity.log" 2>&1
fid_rc=$?
set -e
[ "$fid_rc" = "8" ] || { echo "FAIL: restore --strict non 1:1 esce $fid_rc, atteso 8"; sed -n '1,40p' "$work/fidelity.log"; exit 1; }
# The tree is on disk all the same: --strict changes the verdict, not the work.
[ -p "$work/fifo-strict/$(basename "$ftree")/pipe" ] || { echo "FAIL: --strict ha lasciato la destinazione incompleta"; exit 1; }
[ "$(cat "$work/fifo-strict/$(basename "$ftree")/a.txt")" = "dati" ] || { echo "FAIL: --strict ha perso il contenuto dei file"; exit 1; }
grep -q 'NON 1:1' "$work/fidelity.log" || { echo "FAIL: il verdetto non è stato stampato"; exit 1; }
echo "A8.7 restore --strict non 1:1: esce 8, l'albero resta completo, il verdetto è stampato: OK"

# 4c. the control: a restore that really is 1:1 exits 0 under --strict.
set +e
bin/backimage restore "$REF" --oci-layout "$work/layout" \
	--extract -C "$work/faithful" --passphrase-file "$work/pass.txt" \
	--no-preserve-owner --strict >"$work/faithful.log" 2>&1
ok_rc=$?
set -e
[ "$ok_rc" = "0" ] || { echo "FAIL: un restore fedele con --strict esce $ok_rc, atteso 0"; sed -n '1,40p' "$work/faithful.log"; exit 1; }
grep -q 'esito 1:1' "$work/faithful.log" || { echo "FAIL: il verdetto 1:1 non è stato stampato"; exit 1; }
echo "A8.8 restore fedele con --strict: esce 0 e dichiara 1:1: OK"

# ---------------------------------------------------------------------------
# 5. the closing verdict, and the commands the backup told the user to run
# ---------------------------------------------------------------------------
# A8.8 already asserted exit 0 on a faithful strict restore. What must also be
# there is the one line a person greps in a cron mail.
grep -q 'ESITO: estrazione 1:1, nessun errore' "$work/faithful.log" || {
	echo "FAIL: il restore fedele non stampa il verdetto finale"; sed -n '1,40p' "$work/faithful.log"; exit 1; }
grep -q 'tutti i chunk verificati' "$work/faithful.log" || {
	echo "FAIL: il verdetto non dichiara la verifica dei chunk"; exit 1; }
# And the degraded one must never claim there were no errors.
grep -q 'ESITO: estrazione NON 1:1' "$work/fidelity.log" || {
	echo "FAIL: il restore degradato non stampa il verdetto negativo"; exit 1; }
grep -q 'ESITO: estrazione 1:1, nessun errore' "$work/fidelity.log" && {
	echo "FAIL: un restore degradato dichiara 'nessun errore'"; exit 1; }
echo "A8.9 verdetto finale presente e coerente in entrambi gli esiti: OK"

# The recovery block printed after the backup is pasted verbatim by users, so
# every option in it has to exist on the command it is attributed to.
tips=$(sed -n '/comandi per recuperare i dati/,$p' "$work/backup.log")
[ -n "$tips" ] || { echo "FAIL: il backup non ha stampato i comandi di recupero"; exit 1; }
grep -q -- '--extract --destination ./restore --strict' <<<"$tips" || {
	echo "FAIL: il comando backimage stampato non è a fedeltà massima (manca --strict)"; exit 1; }
for dead in '/var/run/docker.sock' 'BACKIMAGE_IMAGE_REF'; do
	grep -q -- "$dead" <<<"$tips" && { echo "FAIL: i comandi stampati citano ancora $dead"; exit 1; }
done
echo "A8.10 comandi di recupero stampati: --strict presente, socket Docker assente: OK"

# ---------------------------------------------------------------------------
# 6. the same verdict out of the self-extracting image
# ---------------------------------------------------------------------------
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
	echo "A8.11 immagine autoestraente: SKIPPED (docker non disponibile)"
	echo "phase A8 e2e OK"
	exit 0
fi

IMG=backimage-e2e-a8/selfextract:t1
docker rmi -f "$IMG" >/dev/null 2>&1 || true
imgcleanup() { docker rmi -f "$IMG" >/dev/null 2>&1 || true; }
trap 'imgcleanup; cleanup' EXIT

bin/backimage backup "$tree" --repo backimage-e2e-a8/selfextract --tag t1 \
	--passphrase-file "$work/pass.txt" --output daemon --temp-dir "$work/tmp" \
	--allow-degraded --platform linux/amd64 >"$work/image-backup.log" 2>&1

# The docker command the backup itself printed, run as printed.
mkdir -p "$work/img-restore"
BACKIMAGE_PASSPHRASE="$secret" docker run --rm --privileged \
	-e BACKIMAGE_PASSPHRASE -v "$work/img-restore:/restore" \
	"$IMG" extract --out /restore --strict >"$work/image-extract.log" 2>&1
img_rc=$?
[ "$img_rc" = "0" ] || { echo "FAIL: il comando docker stampato esce $img_rc"; sed -n '1,40p' "$work/image-extract.log"; exit 1; }
grep -q 'ESITO: estrazione 1:1, nessun errore' "$work/image-extract.log" || {
	echo "FAIL: l'immagine non stampa il verdetto finale"; sed -n '1,40p' "$work/image-extract.log"; exit 1; }
# Same string as the host binary: one thing to grep, whichever path was used.
grep -q 'ESITO: estrazione 1:1, nessun errore' "$work/faithful.log" || { echo "FAIL: verdetti divergenti"; exit 1; }
# As root inside --privileged the image restores ownership too, so the tree
# must come back identical to the source. Compared with the same snapshot used
# in A8.1 — mode, uid, gid, size, mtime, nlink, content, link target and
# extended attributes — minus the two fields a copy can never reproduce: the
# inode number, and ctime, which no archiver can set. diff -r is not enough
# here: it dereferences symlinks and cannot see a dangling one at all.
norm() { cut -d'|' -f1-7,10- "$1"; }
snapshot "$tree" >"$work/img-src.txt"
snapshot "$work/img-restore/$(basename "$tree")" >"$work/img-dst.txt"
norm "$work/img-src.txt" >"$work/img-src.norm"
norm "$work/img-dst.txt" >"$work/img-dst.norm"
if ! diff -u "$work/img-src.norm" "$work/img-dst.norm" >"$work/img-diff.txt"; then
	echo "FAIL: l'immagine non ha ripristinato l'albero identico"
	sed -n '1,30p' "$work/img-diff.txt"
	exit 1
fi
echo "A8.11 immagine autoestraente: stesso verdetto del binario, albero identico: OK"

echo "phase A8 e2e OK"

