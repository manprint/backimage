//go:build freebsd

package archive

import "golang.org/x/sys/unix"

// mknodAt and mkfifoAt fall back to the pathname form, as on the other
// non-Linux systems (see nodeat_other_unix.go). FreeBSD's mknod takes a 64-bit
// device number.
func mknodAt(_ int, _, full string, mode uint32, dev int) error {
	return unix.Mknod(full, mode, uint64(dev)) // #nosec G115 -- dev is a kernel rdev
}

func mkfifoAt(_ int, _, full string, mode uint32) error {
	return unix.Mkfifo(full, mode)
}
