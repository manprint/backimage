//go:build linux

package archive

import (
	"os"
	"strconv"
)

// xattrPath returns a path through which the attributes of src can be read
// without resolving any component above the entry again: the directory
// handle the walk listed it from, reached through /proc/self/fd, then the
// entry's own name. release must be called once the reads are done.
//
// Without /proc the full path is the only name the kernel offers for a
// by-path read; the caller's identity checks around the read are then what
// keeps a swapped parent from going unnoticed.
func xattrPath(src source) (path string, release func(), err error) {
	if src.dir == nil {
		return src.path, func() {}, nil
	}
	d, err := src.dir.Open(".")
	if err != nil {
		return "", nil, err
	}
	fdDir := "/proc/self/fd/" + strconv.Itoa(int(d.Fd()))
	if _, err := os.Lstat(fdDir); err != nil {
		_ = d.Close()
		return src.path, func() {}, nil
	}
	return fdDir + "/" + src.name, func() { _ = d.Close() }, nil
}
