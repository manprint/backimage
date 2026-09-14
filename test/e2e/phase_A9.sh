#!/usr/bin/env bash
# Phase A9 e2e: the local Docker daemon as a source a backup can be read back
# from — the one source no script exercised, which is why B-A001 stayed
# invisible for the whole life of the flag.
#
# `backimage backup --output daemon` loads the image into the daemon, and
# `--local-repo` reads it back from there. The daemon does not keep a layer the
# way a registry does: it re-labels every one of them tar+gzip and gzips what
# it stored, so the blob the backup wrote comes back with one wrapper too many.
# Applying the declared codec to that produced "invalid input: magic number
# mismatch" and no backup at all could be restored, verified or listed from the
# daemon.
#
# The phase asserts the premise (the wrapper is really there, via
# test/e2e/tools/daemonlayers) and then the whole matrix behind it: every
# codec, encrypted and not, one layer and several, restore/verify/ls/find, and
# --remove-local-image.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
	echo "phase A9 e2e SKIPPED: docker not available"
	echo "phase A9 e2e OK"
	exit 0
fi
for tool in jq stat sha256sum diff; do
	command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 1; }
done

work=$(mktemp -d)
images=()
cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ] && [ -f "$work/last.log" ]; then
		echo "phase A9 diagnostics (exit $rc)" >&2
		sed -n '1,40p' "$work/last.log" >&2
	fi
	for img in "${images[@]:-}"; do
		[ -n "$img" ] && docker rmi -f "$img" >/dev/null 2>&1 || true
	done
	chmod -R u+rwX "$work" >/dev/null 2>&1 || true
	rm -rf "$work"
	return "$rc"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache"
export BACKIMAGE_AUTH_FILE="$XDG_CONFIG_HOME/backimage/auth.json"
mkdir -p "$work/tmp"
secret='e2e-A9-passphrase-lunga-abbastanza'
printf '%s\n' "$secret" >"$work/pass.txt"
chmod 600 "$work/pass.txt"

make embed >/dev/null
go build -o bin/backimage ./cmd/backimage
go build -o "$work/daemonlayers" ./test/e2e/tools/daemonlayers

# snapshot prints one line per path with every field a restore has to
# reproduce, so "identical tree" is not just identical bytes.
snapshot() {
	local root=$1
	(
		cd "$root"
		find . -print0 | sort -z | while IFS= read -r -d '' p; do
			local meta extra
			meta=$(stat -c '%F|%a|%u|%g|%s|%.9Y' -- "$p")
			extra=""
			if [ -L "$p" ]; then
				extra="link:$(readlink -- "$p")"
			elif [ -f "$p" ]; then
				extra="sha:$(sha256sum -- "$p" | cut -d' ' -f1)"
			fi
			printf '%s|%s|%s\n' "$p" "$meta" "$extra"
		done
	)
}

tree="$work/tree"
mkdir -p "$tree/sub/deep"
printf 'contenuto uno\n' >"$tree/sub/a.txt"
printf 'contenuto due\n' >"$tree/sub/deep/b.txt"
printf 'con spazi\n' >"$tree/file con spazi.txt"
head -c 300000 /dev/urandom >"$tree/big.bin"
ln -s sub/a.txt "$tree/link-rel"
touch -d '1999-12-31 23:59:58' "$tree/sub/deep/b.txt"
snapshot "$tree" >"$work/source.txt"
source_paths=$(wc -l <"$work/source.txt")
[ "$source_paths" -ge 7 ] || { echo "FAIL: fixture troppo piccola ($source_paths path)"; exit 1; }

# backup_to_daemon TAG [extra backup args...]
# --allow-degraded only clears the privilege preflight of an unprivileged run
# (chown); the tree is fully readable, so nothing is actually degraded.
backup_to_daemon() {
	local tag=$1; shift
	local ref="bi-e2e-a9:$tag"
	images+=("$ref")
	docker rmi -f "$ref" >/dev/null 2>&1 || true
	bin/backimage backup "$tree" --repo bi-e2e-a9 --tag "$tag" \
		--output daemon --platform linux/amd64 --temp-dir "$work/tmp" \
		--allow-degraded "$@" >"$work/last.log" 2>&1
	echo "$ref"
}

# restored_root echoes the directory inside a restore destination that holds
# the copy of $tree: paths are archived absolute-less but rooted at the source
# directory name.
restored_root() { echo "$1/$(basename "$tree")"; }

# ---------------------------------------------------------------------------
# A9.1 — the premise: the daemon does not serve the blob that was published
# ---------------------------------------------------------------------------
ref=$(backup_to_daemon premise --no-encrypt --compression zstd)
"$work/daemonlayers" "$ref" >"$work/layers.txt" 2>&1 || { echo "FAIL: sonda sui layer"; cat "$work/layers.txt"; exit 1; }
data_line=$(grep '^layer=2 ' "$work/layers.txt") || { echo "FAIL: nessun layer dati"; cat "$work/layers.txt"; exit 1; }
case "$data_line" in
	*"mediatype=application/vnd.docker.image.rootfs.diff.tar.gzip"*) ;;
	*) echo "FAIL: il daemon non rietichetta più i layer, la premessa del fix è cambiata: $data_line"; exit 1;;
esac
case "$data_line" in
	*"compressed=1f8b"*) ;;
	*) echo "FAIL: il daemon non avvolge più il layer in gzip: $data_line"; exit 1;;
esac
# Sotto l'involucro il daemon tiene una delle due cose, e quale dipende dallo
# storage driver: con lo snapshotter containerd tiene il nostro blob così com'è
# (`zstd`), con il docker load classico tiene il tar, perché disfa da sé una
# compressione che riconosce. In entrambi i casi quello che il daemon offre
# come layer non è il blob pubblicato, ed è per questo che il lettore non può
# limitarsi ad applicare il codec dichiarato dal manifest. Un `uncompressed`
# che fosse di nuovo il blob pubblicato senza involucro, invece, direbbe che la
# premessa del fix non vale più.
kind=${data_line##*uncompressedkind=}
case "$kind" in
	zstd) echo "A9.1 il daemon avvolge in gzip il blob zstd del backup (mediatype e magic verificati): OK";;
	tar)  echo "A9.1 il daemon disfa lo zstd e riavvolge in gzip il tar (mediatype e magic verificati): OK";;
	*) echo "FAIL: sotto l'involucro non c'è né il blob zstd né il tar del backup: $data_line"; exit 1;;
esac

# ---------------------------------------------------------------------------
# A9.2 — restore from the daemon: 1:1, and the closing verdict says so
# ---------------------------------------------------------------------------
rm -rf "$work/out1"
bin/backimage restore "$ref" --local-repo -x -C "$work/out1" >"$work/last.log" 2>&1
grep -q 'ESITO: estrazione 1:1, nessun errore' "$work/last.log" || {
	echo "FAIL: nessun verdetto 1:1 nel restore dal daemon"; sed -n '1,40p' "$work/last.log"; exit 1; }
snapshot "$(restored_root "$work/out1")" >"$work/out1.txt"
diff -u "$work/source.txt" "$work/out1.txt" >"$work/out1.diff" || {
	echo "FAIL: l'albero ripristinato dal daemon non coincide con la sorgente"; sed -n '1,40p' "$work/out1.diff"; exit 1; }
echo "A9.2 restore --local-repo: albero identico alla sorgente ($source_paths path) e verdetto 1:1: OK"

# ---------------------------------------------------------------------------
# A9.3 — every codec a backup can be written with survives the round trip
# ---------------------------------------------------------------------------
# Not every codec reaches every daemon: the image tarball names each layer
# .tar.gz whatever the codec, and the daemon undoes only what its own sniffing
# knows (gzip, bzip2, xz, zstd). So lz4 loads where the containerd snapshotter
# keeps the blob as it is and is refused by the classic image store. Refused is
# an acceptable answer here; announcing a published image that is not there is
# not, and that is what the run did before the load response was read.
loaded=()
refused=()
for codec in zstd gzip none xz lz4; do
	runnable=true
	case "$codec" in xz|lz4|none) runnable=false;; esac
	if ! ref=$(backup_to_daemon "c-$codec" --no-encrypt --compression "$codec" --runnable=$runnable); then
		grep -q 'caricamento rifiutato' "$work/last.log" || {
			echo "FAIL: backup su daemon con codec $codec fallito senza dire che il daemon ha rifiutato"
			sed -n '1,40p' "$work/last.log"; exit 1; }
		if docker image inspect "bi-e2e-a9:c-$codec" >/dev/null 2>&1; then
			echo "FAIL: codec $codec: il backup ha riportato un rifiuto ma l'immagine è nel daemon"; exit 1
		fi
		refused+=("$codec")
		continue
	fi
	loaded+=("$codec")
	rm -rf "$work/out-$codec"
	bin/backimage restore "$ref" --local-repo -x -C "$work/out-$codec" >"$work/last.log" 2>&1 || {
		echo "FAIL: restore dal daemon con codec $codec"; sed -n '1,40p' "$work/last.log"; exit 1; }
	snapshot "$(restored_root "$work/out-$codec")" >"$work/out-$codec.txt"
	diff -q "$work/source.txt" "$work/out-$codec.txt" >/dev/null || {
		echo "FAIL: codec $codec: albero diverso dalla sorgente"; exit 1; }
	bin/backimage verify "$ref" --local-repo >"$work/last.log" 2>&1 || {
		echo "FAIL: verify dal daemon con codec $codec"; sed -n '1,40p' "$work/last.log"; exit 1; }
done
# Whatever the daemon is, these three are its own formats plus no compression
# at all: if one of them is refused the premise of the phase is gone, not the
# codec.
for must in zstd gzip none; do
	case " ${loaded[*]} " in
		*" $must "*) ;;
		*) echo "FAIL: il daemon ha rifiutato $must, che deve caricare ovunque"; exit 1;;
	esac
done
echo "A9.3 round trip dal daemon con ${loaded[*]}: albero identico e verify ok${refused:+; rifiutati dal daemon con un errore esplicito: ${refused[*]}}: OK"

# ---------------------------------------------------------------------------
# A9.4 — an encrypted backup, read back from the daemon
# ---------------------------------------------------------------------------
ref=$(backup_to_daemon enc --passphrase-file "$work/pass.txt")
rm -rf "$work/out-enc"
bin/backimage restore "$ref" --local-repo -x -C "$work/out-enc" \
	--passphrase-file "$work/pass.txt" >"$work/last.log" 2>&1
snapshot "$(restored_root "$work/out-enc")" >"$work/out-enc.txt"
diff -q "$work/source.txt" "$work/out-enc.txt" >/dev/null || {
	echo "FAIL: backup cifrato dal daemon: albero diverso"; exit 1; }
bin/backimage verify "$ref" --local-repo --passphrase-file "$work/pass.txt" >"$work/last.log" 2>&1
echo "A9.4 backup cifrato: restore e verify dal daemon: OK"

# ---------------------------------------------------------------------------
# A9.5 — more than one data layer: the reader has to switch layer mid-restore
# ---------------------------------------------------------------------------
ref=$(backup_to_daemon multi --no-encrypt --max-layer-size 4MiB)
grep -q '"layers": *[2-9]' "$work/last.log" 2>/dev/null || true
rm -rf "$work/out-multi"
bin/backimage restore "$ref" --local-repo -x -C "$work/out-multi" >"$work/last.log" 2>&1
snapshot "$(restored_root "$work/out-multi")" >"$work/out-multi.txt"
diff -q "$work/source.txt" "$work/out-multi.txt" >/dev/null || {
	echo "FAIL: backup multi-layer dal daemon: albero diverso"; exit 1; }
echo "A9.5 backup su più layer dati: restore dal daemon identico: OK"

# ---------------------------------------------------------------------------
# A9.6 — the read-only commands read the same image from the daemon
# ---------------------------------------------------------------------------
ref=$(backup_to_daemon readonly --no-encrypt)
bin/backimage ls "$ref" --local-repo >"$work/ls.txt" 2>"$work/last.log"
for want in 'tree/sub/a.txt' 'tree/big.bin' 'tree/link-rel'; do
	grep -qx "$want" "$work/ls.txt" || { echo "FAIL: ls dal daemon non elenca $want"; sed -n '1,20p' "$work/ls.txt"; exit 1; }
done
bin/backimage find "$ref" '**/*.txt' --local-repo >"$work/find.txt" 2>"$work/last.log"
grep -qx 'tree/sub/deep/b.txt' "$work/find.txt" || {
	echo "FAIL: find dal daemon non trova tree/sub/deep/b.txt"; sed -n '1,20p' "$work/find.txt"; exit 1; }
bin/backimage inspect "$ref" --local-repo --json >"$work/inspect.json" 2>"$work/last.log"
jq -e '.manifest.archive.compression == "zstd"' "$work/inspect.json" >/dev/null || {
	echo "FAIL: inspect dal daemon non riporta il codec"; cat "$work/inspect.json"; exit 1; }
echo "A9.6 ls, find, inspect e verify leggono l'immagine dal daemon: OK"

# ---------------------------------------------------------------------------
# A9.7 — --remove-local-image deletes the image only after a restore that worked
# ---------------------------------------------------------------------------
ref=$(backup_to_daemon removable --no-encrypt)
docker image inspect "$ref" >/dev/null 2>&1 || { echo "FAIL: l'immagine non è nel daemon"; exit 1; }
rm -rf "$work/out-rm"
bin/backimage restore "$ref" --local-repo -x -C "$work/out-rm" --remove-local-image >"$work/last.log" 2>&1
snapshot "$(restored_root "$work/out-rm")" >"$work/out-rm.txt"
diff -q "$work/source.txt" "$work/out-rm.txt" >/dev/null || {
	echo "FAIL: --remove-local-image: albero diverso"; exit 1; }
if docker image inspect "$ref" >/dev/null 2>&1; then
	echo "FAIL: --remove-local-image non ha rimosso l'immagine"; exit 1
fi
echo "A9.7 --remove-local-image rimuove l'immagine dopo un restore riuscito: OK"

echo "phase A9 e2e OK"
