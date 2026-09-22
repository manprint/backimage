//go:build unix

package archive

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// readMeta fills the platform-specific fields of e from fi and path. f, when
// not nil, is the checked descriptor of the entry: its extended attributes
// are then read from it rather than by path, so they belong to the same
// object as the content.
func readMeta(path string, f *os.File, fi os.FileInfo, opts Options, e *Entry) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %q: no platform metadata", path)
	}
	e.UID = int(st.Uid)
	e.GID = int(st.Gid)
	e.DevMajor = int64(unix.Major(uint64(st.Rdev)))
	e.DevMinor = int64(unix.Minor(uint64(st.Rdev)))
	e.ModTime = fi.ModTime()
	e.AccessTime, e.ChangeTime = statTimes(st)
	if !opts.NumericOwner {
		e.Uname, e.Gname = resolveOwner(e.UID, e.GID)
	}
	if opts.PreserveXattrs {
		var xs map[string][]byte
		var err error
		if f != nil {
			xs, err = readXattrsFile(path, f)
		} else {
			xs, err = readXattrs(path)
		}
		if err != nil {
			// Only the attributes are lost, and only for this path: the
			// caller decides whether that is fatal. Dropping the entry
			// instead would turn an unreadable attribute into a missing
			// file.
			return &xattrLossError{Path: path, Err: err}
		}
		if len(xs) > 0 {
			e.Xattrs = xs
		}
	}
	return nil
}

// xattrSource reads the attributes of one entry, either through an open
// descriptor or by path without following a final symlink.
type xattrSource struct {
	path string
	fd   int // -1: by path
}

func (s xattrSource) list(buf []byte) (int, error) {
	if s.fd >= 0 {
		return unix.Flistxattr(s.fd, buf)
	}
	return unix.Llistxattr(s.path, buf)
}

func (s xattrSource) get(name string, buf []byte) (int, error) {
	if s.fd >= 0 {
		return unix.Fgetxattr(s.fd, name, buf)
	}
	return unix.Lgetxattr(s.path, name, buf)
}

// readXattrs returns all extended attributes of path, following no symlinks.
func readXattrs(path string) (map[string][]byte, error) {
	return readXattrsFrom(xattrSource{path: path, fd: -1})
}

// readXattrsFile returns all extended attributes of the open file f; path is
// only named in errors.
func readXattrsFile(path string, f *os.File) (map[string][]byte, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("xattrs %s: %w", path, err)
	}
	var (
		xs      map[string][]byte
		readErr error
	)
	if err := rc.Control(func(fd uintptr) {
		xs, readErr = readXattrsFrom(xattrSource{path: path, fd: int(fd)})
	}); err != nil {
		return nil, fmt.Errorf("xattrs %s: %w", path, err)
	}
	return xs, readErr
}

func readXattrsFrom(src xattrSource) (map[string][]byte, error) {
	path := src.path
	// List, growing the buffer on ERANGE (max 3 attempts).
	size, err := src.list(nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			return nil, nil // filesystem without xattr support
		}
		return nil, fmt.Errorf("Llistxattr %s: %w", path, err)
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	attempts := 0
	for {
		n, err := src.list(buf)
		if errors.Is(err, unix.ERANGE) && attempts < 3 {
			attempts++
			buf = make([]byte, len(buf)*2)
			continue
		}
		if err != nil {
			if errors.Is(err, unix.ENOTSUP) {
				return nil, nil
			}
			return nil, fmt.Errorf("Llistxattr %s: %w", path, err)
		}
		buf = buf[:n]
		break
	}
	out := make(map[string][]byte)
	for _, name := range splitNul(buf) {
		if name == "" {
			continue
		}
		val, err := readOneXattr(src, name)
		if err != nil {
			return nil, err
		}
		out[name] = val
	}
	return out, nil
}

func readOneXattr(src xattrSource, name string) ([]byte, error) {
	path := src.path
	size, err := src.get(name, nil)
	if err != nil {
		if isMissingXattr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("Lgetxattr %s.%s: %w", path, name, err)
	}
	if size == 0 {
		return []byte{}, nil
	}
	val := make([]byte, size)
	attempts := 0
	for {
		n, err := src.get(name, val)
		if errors.Is(err, unix.ERANGE) && attempts < 3 {
			attempts++
			val = make([]byte, len(val)*2)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("Lgetxattr %s.%s: %w", path, name, err)
		}
		return val[:n], nil
	}
}

func splitNul(b []byte) []string {
	var names []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				names = append(names, string(b[start:i]))
			}
			start = i + 1
		}
	}
	return names
}

var ownerCache sync.Map // uint64(uid)<<32|gid -> [2]string

// resolveOwner returns the user and group names for uid/gid, cached.
func resolveOwner(uid, gid int) (uname, gname string) {
	key := uint64(uint32(uid))<<32 | uint64(uint32(gid)) // #nosec G115 -- uid/gid bounded by kernel limits
	if v, ok := ownerCache.Load(key); ok {
		pair := v.([2]string)
		return pair[0], pair[1]
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err == nil {
		uname = u.Username
	}
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err == nil {
		gname = g.Name
	}
	ownerCache.Store(key, [2]string{uname, gname})
	return uname, gname
}
