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

## Other documented behaviours

- Sparse files are archived **densely** in this phase (holes become zeroes);
  hole-aware writing is out of scope.
- Directories created on the fly for manipulated archives get `0700` and are
  re-fixed by the final pass.