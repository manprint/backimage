//go:build unix || windows

package archive

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

// errSourceChanged reports an entry that is no longer the object the walk
// examined: renamed over, or swapped for a symlink, a FIFO or a device between
// the lstat that classified it and the open that reads it. Reading it anyway
// would archive bytes from somewhere else under this entry's name and
// metadata — a file outside the backup root, when the swap is a symlink.
var errSourceChanged = errors.New("replaced while archiving")

// source locates one entry of the tree being archived: the handle of the
// directory that listed it, and its name in that directory. Every access to
// the entry goes through that handle, so no path component above the entry is
// resolved again after the walk has vouched for it. A directory swapped for a
// symlink mid-walk therefore cannot redirect the reads below it: the old
// handle still names the directory that was listed.
//
// A nil dir is a root given by the caller, opened by its path: that path is
// the caller's own statement of what to archive.
type source struct {
	dir  *os.Root
	name string
	path string // the entry's filesystem path, for messages and path-only reads
}

func (s source) lstat() (os.FileInfo, error) {
	if s.dir == nil {
		return os.Lstat(s.path)
	}
	return s.dir.Lstat(s.name)
}

func (s source) readlink() (string, error) {
	if s.dir == nil {
		return os.Readlink(s.path)
	}
	return s.dir.Readlink(s.name)
}

// open opens the entry for reading, never blocking on a FIFO or a device and
// never updating its access time where the platform allows it. The caller
// must check the result with verifySource before reading from it.
func (s source) open() (*os.File, error) {
	return openSource(s.dir, s.name, s.path)
}

// openVerified opens the entry and checks that it is still the object st
// describes. On success the descriptor reads like any regular open.
func (s source) openVerified(st os.FileInfo) (*os.File, error) {
	f, err := s.open()
	if err != nil {
		return nil, err
	}
	if err := verifySource(f, st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := settleSource(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return f, nil
}

// verifySource checks that the open descriptor f is the object that st, the
// lstat taken when the walk met the entry, describes: same type, same inode.
// The open itself cannot be trusted to have refused a swap: os.Root follows a
// symlink that stays inside the root, and a rename-over leaves a regular file
// at the path.
func verifySource(f *os.File, st os.FileInfo) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !sameEntry(fi, st) {
		return errSourceChanged
	}
	return nil
}

func sameEntry(a, b os.FileInfo) bool {
	return a.Mode().Type() == b.Mode().Type() && sameInode(a, b)
}

// listing is a directory the walk descends into.
type listing struct {
	dir   *os.Root // what the children are resolved against
	file  *os.File // the directory itself: its identity and its attributes
	names []string // sorted
}

// releaseFile closes the directory's own descriptor once its entry has been
// archived, so a walk holds one descriptor per level of depth, not two.
func (l *listing) releaseFile() {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

func (l *listing) close() {
	l.releaseFile()
	_ = l.dir.Close()
}

// list opens the directory s, which st says is a directory, and reads its
// names. Both handles are checked against st.
func (s source) list(st os.FileInfo) (*listing, error) {
	f, err := s.openVerified(st)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	var dir *os.Root
	if s.dir == nil {
		dir, err = os.OpenRoot(s.path)
	} else {
		dir, err = s.dir.OpenRoot(s.name)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	// The second open resolves the name again, so it is checked too. A
	// directory without search permission cannot be statted through its own
	// handle; it cannot resolve any child through it either, so nothing can be
	// read through a handle that could not be checked.
	if fi, err := dir.Stat("."); err == nil {
		if !sameEntry(fi, st) {
			_ = f.Close()
			_ = dir.Close()
			return nil, errSourceChanged
		}
	} else if !errors.Is(err, os.ErrPermission) {
		_ = f.Close()
		_ = dir.Close()
		return nil, err
	}
	sort.Strings(names)
	return &listing{dir: dir, file: f, names: names}, nil
}
