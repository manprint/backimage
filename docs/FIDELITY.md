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
| hardlinks | ✅ | ✅ | ✅ (reflinks fallback) |
| symlinks | ✅ | ✅ | ❌ (dangling allowed) |
| devices / fifos | ✅ (root) | ✅ | n/a |
| atime/ctime round-trip | only with `PreserveTimes` | same | ctime not supported |

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

## Other documented behaviours

- Sparse files are archived **densely** in this phase (holes become zeroes);
  hole-aware writing is out of scope.
- Directories created on the fly for manipulated archives get `0700` and are
  re-fixed by the final pass.