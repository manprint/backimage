//go:build unix

package archive

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// readMeta fills the platform-specific fields of e from fi and path.
func readMeta(path string, fi os.FileInfo, opts Options, e *Entry) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %q: no platform metadata", path)
	}
	e.UID = int(st.Uid)
	e.GID = int(st.Gid)
	e.DevMajor = int64(unix.Major(uint64(st.Rdev)))
	e.DevMinor = int64(unix.Minor(uint64(st.Rdev)))
	e.ModTime = fi.ModTime()
	e.AccessTime = syscallTimespecToTime(st.Atim)
	e.ChangeTime = syscallTimespecToTime(st.Ctim)
	if !opts.NumericOwner {
		e.Uname, e.Gname = resolveOwner(e.UID, e.GID)
	}
	if opts.PreserveXattrs {
		xs, err := readXattrs(path)
		if err != nil {
			return err
		}
		if len(xs) > 0 {
			e.Xattrs = xs
		}
	}
	return nil
}

func syscallTimespecToTime(ts syscall.Timespec) time.Time {
	return time.Unix(ts.Sec, ts.Nsec)
}

// readXattrs returns all extended attributes of path, following no symlinks.
func readXattrs(path string) (map[string][]byte, error) {
	// List, growing the buffer on ERANGE (max 3 attempts).
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		if err == unix.ENOTSUP {
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
		n, err := unix.Llistxattr(path, buf)
		if err == unix.ERANGE && attempts < 3 {
			attempts++
			buf = make([]byte, len(buf)*2)
			continue
		}
		if err != nil {
			if err == unix.ENOTSUP {
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
		val, err := readOneXattr(path, name)
		if err != nil {
			return nil, err
		}
		out[name] = val
	}
	return out, nil
}

func readOneXattr(path, name string) ([]byte, error) {
	size, err := unix.Lgetxattr(path, name, nil)
	if err != nil {
		if err == unix.ENODATA {
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
		n, err := unix.Lgetxattr(path, name, val)
		if err == unix.ERANGE && attempts < 3 {
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
