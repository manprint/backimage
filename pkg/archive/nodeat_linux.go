//go:build linux

package archive

import "golang.org/x/sys/unix"

// mknodAt and mkfifoAt create a node named base inside the directory dirfd
// refers to. full is unused here and exists only so the non-Linux build can
// fall back to a pathname; see nodeat_other_unix.go.
func mknodAt(dirfd int, base, _ string, mode uint32, dev int) error {
	return unix.Mknodat(dirfd, base, mode, dev)
}

func mkfifoAt(dirfd int, base, _ string, mode uint32) error {
	return unix.Mkfifoat(dirfd, base, mode)
}
