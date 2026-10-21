//go:build freebsd

package fixtures

import "golang.org/x/sys/unix"

// mknod creates a device node; FreeBSD takes the device number as uint64.
func mknod(path string, mode uint32, dev uint64) error { return unix.Mknod(path, mode, dev) }
