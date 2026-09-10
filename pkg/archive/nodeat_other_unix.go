//go:build unix && !linux

package archive

import "golang.org/x/sys/unix"

// mknodAt and mkfifoAt fall back to the pathname form outside Linux.
//
// Darwin has no mknodat and no mkfifoat syscall at all, and the BSDs cover
// only part of the pair, so there is nothing to anchor these two calls to.
// Everything else in the extraction still goes through the destination's
// os.Root: the residual gap is the final component of a device or fifo, whose
// containing directory was already resolved through that root. Both calls also
// need privileges that a restore rarely has outside Linux.
func mknodAt(_ int, _, full string, mode uint32, dev int) error {
	return unix.Mknod(full, mode, dev)
}

func mkfifoAt(_ int, _, full string, mode uint32) error {
	return unix.Mkfifo(full, mode)
}
