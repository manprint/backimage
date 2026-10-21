#!/usr/bin/env bash
# Phase 01 e2e: archive/extract round-trip fidelity via the archive package.
#
#   make e2e PHASE=01            # non-root: exercises basic metadata
#   sudo make e2e PHASE=01       # root: also devices, ownership, ACLs
#
# Exits 1 at the first difference; prints the diff. Privilege-gated parts are
# skipped (SKIP:) rather than failed when run without root.
set -euo pipefail
root=$(pwd)
cd "$(dirname "$0")/../.."

work=$(mktemp -d)
immutable=""
cleanup() {
	# An immutable file cannot be removed, and neither can its directory: lift
	# the flag first or the cleanup fails the run after it has passed.
	if [ -n "$immutable" ]; then
		chattr -i "$immutable" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
src="$work/src"
dst="$work/dst"
tar="$work/tree.tar"
mkdir -p "$src"

# --- fixture tree -------------------------------------------------------------
mkdir -p "$src/dir/sub"
echo hello >"$src/dir/file.txt"
echo world >"$src/dir/sub/deep.txt"
ln -s ../dir/file.txt "$src/dirlink"
ln -s /nonexistent "$src/broken-link"
ln "$src/dir/file.txt" "$src/dir/hardlink.txt"
chmod 0640 "$src/dir/file.txt"
chmod 0755 "$src/dir/sub"
chmod 0700 "$src/dir/sub/deep.txt"
xattr_set=""
if command -v setfattr >/dev/null && command -v getfattr >/dev/null; then
	if setfattr -n user.demo -v "value-01" "$src/dir/file.txt" 2>/dev/null; then
		xattr_set=1
	else
		echo "SKIP: user xattrs not supported on $work"
	fi
fi
if command -v setcap >/dev/null && [ "$(id -u)" -eq 0 ]; then
	setcap cap_net_bind_service=ep "$src/dir/file.txt" 2>/dev/null && echo "root: capabilities fixture created"
fi
if [ "$(id -u)" -eq 0 ]; then
	mknod "$src/dir/chardev" c 1 3 2>/dev/null || echo "SKIP: mknod char device"
	mknod "$src/dir/blockdev" b 7 0 2>/dev/null || echo "SKIP: mknod block device"
	mkfifo "$src/dir/fifo" 2>/dev/null || echo "SKIP: mkfifo"
	# Ownership before the immutable flag: chattr +i forbids chown too.
	if ! chown 65534:65534 "$src/dir/sub/deep.txt"; then
		echo "FAIL: chown as root"
		exit 1
	fi
	if chattr +i "$src/dir/sub/deep.txt" 2>/dev/null; then
		immutable="$src/dir/sub/deep.txt"
	else
		echo "SKIP: chattr +i not supported on $work"
	fi
else
	echo "SKIP: device/ownership/immutable fixtures require root"
fi

# --- round trip ---------------------------------------------------------------
mkdir -p "$dst"
go run ./test/e2e/tools/archiveroundtrip "$src" "$tar" "$dst"

# --- comparison ---------------------------------------------------------------
echo "== diff -r --no-dereference"
# diff cannot compare device nodes and FIFOs and reports them as different
# even when they match: their type and device number are compared below.
if ! diff -r --no-dereference -x chardev -x blockdev -x fifo \
	"$src" "$dst/$(basename "$src")" >"$work/diff.txt" 2>&1; then
	cat "$work/diff.txt"
	echo "FAIL: round-trip differs"
	exit 1
fi
echo OK

# diff -r compares content and link targets only: owner, group, mode, type and
# device number are compared here, or a restore that dropped them would pass.
echo "== owner, group, mode, type and device number"
meta() { (cd "$1" && find . -exec stat -c '%n %u:%g %a %F %t:%T' {} + | LC_ALL=C sort); }
if ! diff <(meta "$src") <(meta "$dst/$(basename "$src")") >"$work/meta.txt"; then
	cat "$work/meta.txt"
	echo "FAIL: ownership or mode differs"
	exit 1
fi
if [ "$(id -u)" -eq 0 ] && [ "$(stat -c %u:%g "$dst/$(basename "$src")/dir/sub/deep.txt")" != "65534:65534" ]; then
	echo "FAIL: dir/sub/deep.txt lost its owner"
	exit 1
fi
echo OK

if [ -n "$xattr_set" ]; then
	got=$(getfattr -h --only-values -n user.demo "$dst/$(basename "$src")/dir/file.txt" 2>/dev/null || true)
	if [ "$got" != "value-01" ]; then
		echo "FAIL: user.demo not restored (got '$got')"
		exit 1
	fi
fi

if command -v getfattr >/dev/null; then
	echo "== getfattr -d -R"
	norm() { sed 's|# file: .*|# file: REL|'; }
	fa=$(cd "$src" && getfattr -h -d -R . 2>/dev/null | sort | norm)
	fb=$(cd "$dst/$(basename "$src")" && getfattr -h -d -R . 2>/dev/null | sort | norm)
	if [ "$fa" != "$fb" ]; then
		diff <(echo "$fa") <(echo "$fb") || true
		echo "FAIL: xattrs differ"
		exit 1
	fi
	echo OK
else
	echo "SKIP: getfattr not installed"
fi

if command -v getfacl >/dev/null; then
	echo "== getfacl -R"
	fa=$(getfacl -R -p "$src" 2>/dev/null | grep -v '^# file: ' | sort)
	fb=$(getfacl -R -p "$dst/$(basename "$src")" 2>/dev/null | grep -v '^# file: ' | sort)
	if [ "$fa" != "$fb" ]; then
		diff <(echo "$fa") <(echo "$fb") || true
		echo "FAIL: ACLs differ"
		exit 1
	fi
	echo OK
else
	echo "SKIP: getfacl not installed"
fi

echo "phase 01 e2e OK"
