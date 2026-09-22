# Fidelity guarantees

This document lists the exact checks `fixtures.CompareTrees` performs and the
only two relaxations allowed in round-trip tests.

## Compared attributes

For every path present in **both** trees:

- presence / absence (missing paths and unexpected extra paths),
- type (regular, dir, symlink, device, fifo, …),
- content (SHA-256) for regular files,
- symlink target,
- permission bits `0o7777` (including setuid, setgid, sticky),
- uid / gid (unless `CompareOptions.IgnoreOwner`),
- mtime (nanosecond precision),
- device numbers (`rdev`) for device nodes,
- extended attributes in all namespaces (unless `CompareOptions.IgnoreXattrs`),
- hardlink groups: paths that shared an inode in the source must share an
  inode in the copy. Matching is topology-based (path sets), never inode-based,
  because a copy always allocates new inodes.

## Relaxations (the only two allowed)

| Attribute | Default | How to enable comparison |
|-----------|---------|--------------------------|
| atime | **not compared** | `CompareOptions.CompareAccessTime: true` |
| ctime | **never compared** | ctime cannot be restored by any archiver |

Both relaxations are deliberate: no archiver can restore ctime, and atime is
mutable noise for backup workloads.

## Additional switches

- `IgnoreOwner`: for Windows round-trips (owner identity lives in the security
  descriptor, uid/gid are `0`).
- `IgnoreXattrs` — platforms without extended attributes.
- `IgnoreACLs` — strips `system.posix_acl_*` and `security.selinux` from the
  comparison (used when the test creates the ACL via chmod but the extraction
  cannot recreate kernel-identical SELinux labels).

## Preserved metadata, by platform

| Property | Linux | macOS | Windows |
|----------|-------|-------|---------|
| regular content | ✅ | ✅ | ✅ |
| permission bits (incl. setuid/setgid/sticky) | ✅ | ✅ | ❌ (mapped to read-only/archive bit) |
| uid / gid + names | ✅ | ✅ | ❌ (identity lives in the security descriptor) |
| mtime (ns) | ✅ | ✅ | ✅ (100 ns FILETIME) |
| atime | only with `PreserveTimes` | only with `PreserveTimes` | ✅ |
| ctime | ❌ (impossible for any archiver) | ❌ | ❌ |
| user.* / system.* xattrs | ✅ | ✅ (`com.apple.ResourceFork` included) | ❌ (alternate data streams) |
| POSIX ACLs | ✅ (`system.posix_acl_*`) | ❌ (NFSv4 ACLs are not xattr-backed) | n/a (owner ACLs) |
| security.capability | ✅ (root/CAP_SETFCAP) | n/a | n/a |
| trusted.* xattrs (overlayfs) | archived always, restored only with CAP_SYS_ADMIN | n/a | n/a |
| hardlinks | ✅ | ✅ | restore ✅ (NTFS; copy fallback otherwise), archiving ❌ (see below) |
| symlinks | ✅ | ✅ | ⚠️ requires developer mode or `SeCreateSymbolicLinkPrivilege`, otherwise reported as skipped |
| devices / fifos | ✅ (root) | ✅ | ❌ reported as skipped, never created as empty files |
| names with `\ : * ? " < > \|`, or a trailing space or dot | ✅ | ✅ | ❌ reported as skipped |
| atime/ctime round-trip | only with `PreserveTimes` | same | ctime not supported |

Grouping hard links when archiving needs a stable inode identity for each
path. Linux and macOS expose one through `Stat_t`; Windows exposes none, so a
backup taken **on** Windows stores every link as its own regular file with its
own copy of the payload. Reading such a backup back is unaffected, and a
backup taken on Linux or macOS restores its groups on NTFS.

## Extraction order (mandatory)

1. create the object (file/dir/symlink/device/fifo/hardlink)
2. write content (regular files only)
3. `lchown(uid, gid)` — BEFORE chmod: chown clears setuid/setgid
4. `chmod(mode)` — not for symlinks (no lchmod on Linux)
5. `setxattr(...)` — AFTER chown: `security.capability` is cleared by chown
6. `utimes(atime, mtime)` with lutimes — last among per-entry metadata

After all entries:

7. re-apply mode and timestamps to ALL directories, deepest-first (writing
   into a directory changes its mtime; a 0500 directory is not writable until
   it is fully populated)

## What a hardlink is allowed to point at

A `TypeLink` entry names a file that must already exist. The extractor accepts
only a **regular file this same restore has already written**, resolved inside
the destination.

The name in the header never went through the checks applied to the entry's own
name, so it used to be joined to the destination and linked as-is:
`Linkname="../outside"` gave the restore a second name for a file outside it,
and the metadata pass then rewrote that file's owner, mode and timestamps
through the shared inode — no privileges required.

**Change of fidelity**: a hardlink whose first name is not part of this restore
is now **skipped and reported** (`Stats.Skipped`, `Stats.Errors`, and `skipped`
/ `skipped_reasons` in `restore --json`). Before, it was materialised as a copy
by reading whatever happened to sit at that path on disk. Three cases produce
it:

- the first name was excluded by `--include` / `--exclude`;
- `--strip-components` cut the first name away entirely;
- the first name appears *after* the link in the archive (a forward link).
  backimage's own writer never produces one.

A selective restore rarely hits this: asking for a hardlink also asks for the
name it points at, so the group comes back whole (`selectionSet` in
`pkg/recovery`).

## Extended attributes that cannot be restored

A restore never aborts because of an attribute the destination cannot hold.
Two families are skipped with a warning, even in strict mode, and counted in
`Stats.XattrsSkipped`:

- `trusted.*` refused with `EPERM`/`EACCES`. Writing that namespace requires
  `CAP_SYS_ADMIN` in the initial user namespace, which a container started
  without `--privileged` never has. What lives there in practice is overlayfs
  bookkeeping (`trusted.overlay.opaque`, `.redirect`, `.origin`), produced by
  archiving a tree that contains a nested `/var/lib/docker`. It describes the
  layering of the source filesystem, not user data.
- any namespace the destination filesystem refuses outright (`EOPNOTSUPP` on
  tmpfs/NFS/vfat, `EINVAL` for a prefix the kernel does not know).

`security.*`, `system.*` (ACLs) and `user.*` are degraded the same way by
default, and counted separately per namespace; `--strict` turns any of them
back into a hard failure whose error names the remediation.

Attributes are written through a **descriptor** of the object, not through its
pathname, because there is no `*at` form of `setxattr`. That has one
consequence: only regular files, directories and hardlinks can receive them. A
symlink cannot be opened to be written to, and opening a device node has
effects on the device itself, so extended attributes on symlinks, devices and
fifos are reported as skipped rather than applied through a path that could be
swapped underneath.

## Degradation classes (restore)

Without `--strict`, every metadata operation is best effort. `Stats.Degraded`
counts what was dropped, by class: `owner`, `mode`, `times`, `xattr.<ns>`,
`hardlink` (materialised as an independent copy) and `object` (an entry that
could not be created at all, also counted in `Stats.Skipped` and listed in
`Stats.Errors`).

These abort regardless of the policy, because degrading them would hide a real
problem: `ENOSPC`, `EDQUOT`, `EROFS`, `EIO`, `ENOMEM`, `EMFILE`, `ENFILE`, a
truncated archive, a non-empty destination without `Overwrite`, and an
unsupported typeflag.

## Confinement of the extraction

Every filesystem operation of a restore is resolved through a single
`os.Root` opened on the destination, and never through a pathname rebuilt and
reopened. The check and the use are the same syscall, so a component replaced
by a symlink between them cannot move the operation outside the destination:
there is no "between them" left.

What `os.Root` does not cover — creating a device or a fifo, and the
symlink-safe form of `utimensat` — uses the `*at` syscalls with the descriptor
of the containing directory, obtained through that same root. On targets
without `mknodat`/`mkfifoat` (macOS, and partly the BSDs) the final component
of a device or fifo is created by pathname; its directory was still resolved
through the root.

## Confinement of the backup walk

The backup reads a tree that other users may be writing to while it runs —
the typical case is root archiving `/home`. A walk that resolves every entry
by its pathname reads whatever that pathname names *at the moment of the
read*, and a user who owns a directory in the tree decides that: rename the
directory away after the walk has listed it, put a symlink to `/etc` in its
place, and the names already queued (`shadow`, created in advance as decoys)
resolve to `/etc/shadow`, archived under the user's own path and returned to
them by the next restore. The same user could swap one of their files for a
FIFO and stall the backup forever on `open`.

The walk therefore never resolves a path twice:

- every directory is opened once and every child is looked up in *that*
  handle (`os.Root`, i.e. `openat`/`fstatat` relative to the listed
  directory); a directory swapped after it was listed does not change what
  its handle names;
- every open is `O_NOFOLLOW | O_NONBLOCK`, so a FIFO or device put in place
  of a regular file opens without blocking and without waiting for a writer;
- every descriptor is compared with the `lstat` that classified the entry —
  same type, same device, same inode — before a byte or an attribute is read
  from it. `os.Root` follows a symlink that stays inside the root, so this
  check, not the open flags, is what refuses an in-tree swap;
- an entry that fails the check is *replaced while archiving*: an error in
  strict mode, a regular file archived without content in degraded mode
  (counted in `ContentSkipped`, listed in `Errors`), exactly like a file that
  cannot be opened;
- extended attributes and ACLs of regular files and directories are read from
  the checked descriptor (`flistxattr`/`fgetxattr`), so they belong to the
  same object as the content.

The preflight scan and the estimate walk over the same sources use the same
primitives, and the estimate honours `--exclude` and `--one-file-system`
exactly as the archive does: it no longer walks into a mount point the archive
stops at.

What is still read by pathname: the extended attributes of symlinks, devices
and FIFOs, which cannot be opened without either following the link or acting
on the device. Their type, owner, mode and times come from the handle-relative
lstat, but `llistxattr`/`lgetxattr` resolve the full path again, so a
concurrent swap of an ancestor directory can make those attributes — and only
those, never file content — come from another object.

On Windows the walk uses the same handles, but `os.FileInfo` exposes no file
identity, so only the type of the entry is compared.

## Names the archive can hold and the filesystem cannot

A tar path is slash-separated, so on Unix a backslash is an ordinary character
in a filename. `a\b` is one file, and it is archived, indexed and restored as
one file. Quotes, newlines, tabs, leading and trailing spaces, and both Unicode
normal forms are carried unchanged too.

The one exception is a name that is not valid UTF-8. The tar keeps the real
bytes and the file is archived and restored intact, but `index.json.zst` is
JSON and JSON has no encoding for those bytes: each is replaced with U+FFFD.
`ls`, `find` and a selective restore match against the index, so they will show
and select the wrong name for such a file. A full restore is unaffected.

On Windows a name containing `\ : * ? " < > |`, or ending in a space or a dot,
cannot exist at all: those entries are reported as skipped instead of being
rewritten into a name the backup does not describe.

## Emission order (determinism)

Entries come out of the writer in one order and one only: each directory is
emitted before its children, and the children of **every** directory, the roots
included, are emitted in ascending byte order of their names. Two runs over an
unchanged tree produce byte-identical archives.

This is load-bearing beyond determinism. A hardlink group is stored as one
payload plus `TypeLink` entries pointing at the first name the writer saw, and
the extractor accepts only a name this same restore has already written, so
"first" has to mean the same thing on both sides.

## A file whose content cannot be read

Only `--allow-degraded` reaches this case; a strict backup refuses to start
when the preflight finds unreadable files.

The entry keeps its name, type, mode, owner, timestamps and extended
attributes, and carries an **empty payload**: a tar header whose size does not
match the bytes that follow is a corrupt tar, and dropping the entry would turn
an unreadable file into a missing one. `Entry.SHA256` is therefore the digest
of what the archive holds — the digest of no bytes — never a digest of content
that was never read.

What says the bytes are missing is `Stats.ContentSkipped`, surfaced as
`contentSkipped` in `backup --json` and as a warning line on every run that has
one. A restore materialises those entries as empty files.

Until this was fixed the digest was simply left empty, which the index schema
rejects (`entry[N] bad sha256`): a single unreadable file made the whole backup
fail at the metadata step, after the archive had already been built, compressed
and encrypted — so `--allow-degraded` never worked for the one case it exists
for.

## Other documented behaviours

- Sparse files are archived **densely** in this phase (holes become zeroes);
  hole-aware writing is out of scope.
- Directories created on the fly for manipulated archives get `0700` and are
  re-fixed by the final pass.
- A backup **never writes to the source tree**. The walk uses `fstatat`,
  `openat(O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_NOATIME)`, `getdents`, `fstat`,
  `readlinkat`, `flistxattr`/`fgetxattr` and, for symlinks, devices and FIFOs,
  `llistxattr`/`lgetxattr` — nothing else; every byte the run produces goes to
  `--temp-dir` (default `$TMPDIR`), to the checkpoint store under
  `$XDG_CACHE_HOME`, or to `--output-path`. On Linux files and directories are
  read with `O_NOATIME`, so their access times do not move either. The kernel
  grants `O_NOATIME` only to the file's owner or to a process holding
  `CAP_FOWNER` (root has it): for a file the backup neither owns nor is
  privileged over it falls back to a plain read, and the kernel updates the
  `atime` as `cp`, `tar` or `sha256sum` would. `readlink` always updates the
  `atime` of the symlink itself; no flag avoids it. FIFOs and devices are
  never opened. `pkg/archive` `TestBackupLeavesSourceAccessTimesAlone` locks
  the access times of the preflight, estimate and archive walks.
  `test/e2e/phase_A8.sh` locks this by comparing a full metadata snapshot —
  `ctime` included, the field any metadata write would move — taken before and
  after a real backup.