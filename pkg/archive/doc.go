// Package archive reads and writes deterministic PAX tar archives that
// preserve filesystem metadata integrally: ownership (uid/gid + names),
// permission bits, setuid/setgid/sticky, nanosecond timestamps, xattrs
// (including POSIX ACLs in system.posix_acl_*, capabilities in
// security.capability, SELinux labels), hardlinks, symlinks, device nodes
// and FIFOs.
//
// Invariants:
//   - The tar format is always PAX; headers are never emitted as GNU or
//     USTAR (long names/attrs would force silent truncation).
//   - Archive emission order is deterministic: entries of each directory
//     are sorted by byte-level name, directories precede their content,
//     roots are processed in the order they were added.
//   - Entries whose metadata cannot be read are errors when Strict is
//     true (the default); in degraded mode they are skipped and counted.
//   - atime and ctime are the only metadata that may not survive a
//     round-trip (documented in docs/FIDELITY.md).
//   - On macOS NFSv4 ACLs are not exported through xattrs and are not
//     preserved; POSIX ACLs on Linux live in system.posix_acl_* and are
//     preserved without any extra dependency. Windows metadata is handled
//     through the go-winio backuptar layer and a PAX record
//     MSWINDOWS.rawsd.
package archive
