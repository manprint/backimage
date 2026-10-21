//go:build unix && !freebsd

package fixtures

import "golang.org/x/sys/unix"

// mknod creates a device node; these targets take the device number as int.
func mknod(path string, mode uint32, dev uint64) error { return unix.Mknod(path, mode, int(dev)) }
