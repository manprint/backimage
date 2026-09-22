//go:build unix

package archive

import (
	"errors"
	"os"
	"syscall"
)

// sourceOpenFlags never follow a final symlink and never block: a FIFO put in
// place of a regular file opens at once, and verifySource then refuses it,
// instead of the backup waiting forever for a writer.
const sourceOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func openSource(dir *os.Root, name, path string) (*os.File, error) {
	f, err := openSourceFlags(dir, name, path, sourceOpenFlags|noAtimeFlag)
	if err != nil && noAtimeFlag != 0 && errors.Is(err, syscall.EPERM) {
		// O_NOATIME is refused unless the caller owns the file or holds
		// CAP_FOWNER. Reading without it updates the access time, which is
		// the platform's price for reading at all, not a reason to skip.
		f, err = openSourceFlags(dir, name, path, sourceOpenFlags)
	}
	return f, err
}

func openSourceFlags(dir *os.Root, name, path string, flags int) (*os.File, error) {
	if dir == nil {
		return os.OpenFile(path, flags, 0)
	}
	return dir.OpenFile(name, flags, 0)
}

// settleSource clears O_NONBLOCK once the descriptor is known to be the
// regular file or directory the walk expected: it only had to keep the open
// from blocking, and a filesystem that honours it on reads (some FUSE
// servers) would otherwise fail a copy with EAGAIN.
func settleSource(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := rc.Control(func(fd uintptr) {
		setErr = syscall.SetNonblock(int(fd), false)
	}); err != nil {
		return err
	}
	return setErr
}

func sameInode(a, b os.FileInfo) bool {
	sa, ok := a.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	sb, ok := b.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino
}
