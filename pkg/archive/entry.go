package archive

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

// EntryType classifies a filesystem object.
type EntryType uint8

const (
	TypeRegular EntryType = iota
	TypeDir
	TypeSymlink
	TypeHardlink
	TypeCharDevice
	TypeBlockDevice
	TypeFifo
)

// Entry is the platform-independent description of one filesystem object.
type Entry struct {
	Path       string // slash-separated, relative to the archive root, never absolute, never contains ".."
	Type       EntryType
	Size       int64       // 0 for anything but TypeRegular
	Mode       os.FileMode // permission bits + setuid/setgid/sticky
	UID, GID   int
	Uname      string // may be empty when the name cannot be resolved
	Gname      string
	ModTime    time.Time // nanosecond precision
	AccessTime time.Time
	ChangeTime time.Time
	LinkTarget string // symlink target, or path of the first hardlink occurrence
	DevMajor   int64
	DevMinor   int64
	Xattrs     map[string][]byte // raw values; keys include the namespace, e.g. "user.foo", "system.posix_acl_access"
	TarOffset  int64             // byte offset of this entry's header in the archive (valid after Close)
	SHA256     string            // hex sha256 of the payload, regular files only
}

// Validate reports whether the entry is internally consistent and safe to extract.
func (e *Entry) Validate() error {
	switch {
	case e.Path == "":
		return fmt.Errorf("entry path is empty")
	case strings.HasPrefix(e.Path, "/"):
		return fmt.Errorf("entry path %q is absolute", e.Path)
	case e.Path == ".." || strings.HasPrefix(e.Path, "../") || strings.Contains(e.Path, "/../"):
		return fmt.Errorf("entry path %q contains ..", e.Path)
	case e.Size != 0 && e.Type != TypeRegular:
		return fmt.Errorf("entry %q: size %d on non-regular type %v", e.Path, e.Size, e.Type)
	case (e.Type == TypeSymlink || e.Type == TypeHardlink) && e.LinkTarget == "":
		return fmt.Errorf("entry %q: %v with empty link target", e.Path, e.Type)
	}
	for k := range e.Xattrs {
		if k == "" {
			return fmt.Errorf("entry %q: empty xattr key", e.Path)
		}
	}
	return nil
}

// CleanPath normalises an archive-relative path to slash form.
func CleanPath(p string) string {
	return path.Clean(strings.ReplaceAll(p, "\\", "/"))
}
