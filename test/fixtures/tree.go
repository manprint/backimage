// Package fixtures builds filesystem trees used by archive round-trip tests.
package fixtures

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Feature flags select which hostile cases to include.
type Feature uint32

const (
	FeatBasic     Feature = 1 << iota // regular files, dirs, empty dir, nested dirs
	FeatPerms                         // 0000, 0777, setuid, setgid, sticky
	FeatSymlinks                      // relative, absolute, dangling, symlink to dir
	FeatHardlinks                     // 2 and 3 links to the same inode
	FeatXattrs                        // user.* xattrs, empty value, 4KiB value, binary value
	FeatACLs                          // POSIX ACL via system.posix_acl_access
	FeatCaps                          // security.capability
	FeatDevices                       // char and block devices (requires root)
	FeatFifos                         // named pipes
	FeatOwnership                     // files owned by uid/gid != current (requires root or userns)
	FeatNames                         // unicode, emoji, spaces, newline, 250-byte name, 4096-byte path
	FeatSparse                        // sparse file with a 64 MiB hole
	FeatTimes                         // mtime with nanoseconds, mtime in 1970, mtime in 2200
	FeatBigFile                       // 128 MiB file (skipped in short mode)
)

// RequiresRoot reports which of the requested features need privileges.
func RequiresRoot(feats Feature) Feature {
	return feats & (FeatACLs | FeatCaps | FeatDevices | FeatOwnership)
}

// Manifest describes exactly what was created, for later comparison.
type Manifest struct {
	Files          []string
	Dirs           []string
	Symlinks       []string
	HardlinkGroups [][]string        // each group: the paths sharing one inode
	SHA            map[string]string // path -> sha256hex of content
	Devices        []string
	Fifos          []string
}

// Build materialises the tree under dir and returns a manifest describing
// exactly what was created, for later comparison.
func Build(t *testing.T, dir string, feats Feature) *Manifest {
	t.Helper()
	m := &Manifest{SHA: map[string]string{}}
	mkd := func(rel string) {
		if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		m.Dirs = append(m.Dirs, rel)
	}
	write := func(rel string, content []byte) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil { // #nosec G306 -- fixtures deliberately mirror real-world perms (0644, not secrets)
			t.Fatalf("write %s: %v", rel, err)
		}
		m.Files = append(m.Files, rel)
		h := sha256.Sum256(content)
		m.SHA[rel] = hex.EncodeToString(h[:])
	}
	link := func(rel, target string) {
		if err := os.Symlink(target, filepath.Join(dir, rel)); err != nil {
			t.Fatalf("symlink %s: %v", rel, err)
		}
		m.Symlinks = append(m.Symlinks, rel)
	}

	if feats&FeatBasic != 0 {
		mkd("basic")
		write("basic/hello.txt", []byte("hello world\n"))
		write("basic/empty.txt", nil)
		mkd("basic/nested/deeper")
		write("basic/nested/deeper/file.bin", []byte{0, 1, 2, 3, 4})
		mkd("empty-dir")
	}
	if feats&FeatPerms != 0 {
		mkd("perms")
		write("perms/secret000", []byte(""))
		if err := os.Chmod(filepath.Join(dir, "perms/secret000"), 0); err != nil {
			t.Fatalf("chmod 000: %v", err)
		}
		write("perms/open777", []byte("x"))
		if err := os.Chmod(filepath.Join(dir, "perms/open777"), 0o777); err != nil {
			t.Fatalf("chmod 777: %v", err)
		}
		write("perms/suid", []byte("s"))
		if err := os.Chmod(filepath.Join(dir, "perms/suid"), os.ModeSetuid|0o755); err != nil {
			t.Fatalf("chmod suid: %v", err)
		}
		write("perms/sgid", []byte("g"))
		if err := os.Chmod(filepath.Join(dir, "perms/sgid"), os.ModeSetgid|0o755); err != nil {
			t.Fatalf("chmod sgid: %v", err)
		}
		write("perms/sticky", []byte("t"))
		if err := os.Chmod(filepath.Join(dir, "perms/sticky"), os.ModeSticky|0o755); err != nil {
			t.Fatalf("chmod sticky: %v", err)
		}
	}
	if feats&FeatSymlinks != 0 {
		mkd("sym")
		link("sym/rel", "basic/hello.txt")
		link("sym/abs", "/tmp/backimage-abs-target")
		link("sym/dangling", "no-such-target")
		link("sym/todir", "basic")
	}
	if feats&FeatHardlinks != 0 {
		mkd("hard")
		write("hard/orig.txt", []byte("hard-link content"))
		if err := os.Link(filepath.Join(dir, "hard/orig.txt"), filepath.Join(dir, "hard/link2.txt")); err != nil {
			t.Fatalf("hardlink2: %v", err)
		}
		if err := os.Link(filepath.Join(dir, "hard/orig.txt"), filepath.Join(dir, "hard/link3.txt")); err != nil {
			t.Fatalf("hardlink3: %v", err)
		}
		m.HardlinkGroups = append(m.HardlinkGroups,
			[]string{"hard/orig.txt", "hard/link2.txt", "hard/link3.txt"})
	}
	if feats&FeatXattrs != 0 {
		mkd("xattr")
		write("xattr/empty", []byte("e"))
		write("xattr/binary", []byte("b"))
		write("xattr/big", []byte("y"))
		setXattr(t, filepath.Join(dir, "xattr/empty"), "user.empty", []byte{})
		setXattr(t, filepath.Join(dir, "xattr/binary"), "user.binary",
			[]byte{0x00, 0x01, 0x00, 0xff, 0x0a, 0x00})
		big := make([]byte, 3500)
		for i := range big {
			big[i] = byte(i % 251)
		}
		setXattr(t, filepath.Join(dir, "xattr/big"), "user.big", big)
	}
	if feats&FeatNames != 0 {
		mkd("names")
		write("names/ünïcödé file.txt", []byte("u"))
		write("names/emoji 🙃.txt", []byte("e"))
		write("names/with spaces.txt", []byte("s"))
		write("names/line\nbreak.txt", []byte("l"))
		write("names/"+strings.Repeat("x", 250), []byte("L"))
		deep := ""
		for i := 0; i < 200; i++ {
			deep += "d" + fmt.Sprint(i%10) + "/"
		}
		write("names/"+deep+"end.txt", []byte("E"))
	}
	if feats&FeatTimes != 0 {
		mkd("times")
		write("times/old.txt", []byte("o"))
		write("times/future.txt", []byte("f"))
		write("times/ns.txt", []byte("n"))
		old := time.Unix(0, 123456789)
		future := time.Unix(2200*365*24*3600, 987654321)
		ns := time.Unix(1700000000, 123456789)
		for name, ts := range map[string]time.Time{
			"old.txt":    old,
			"future.txt": future,
			"ns.txt":     ns,
		} {
			if err := os.Chtimes(filepath.Join(dir, "times", name), ts, ts); err != nil {
				t.Fatalf("chtimes %s: %v", name, err)
			}
		}
	}
	if feats&FeatSparse != 0 {
		mkd("sparse")
		p := filepath.Join(dir, "sparse/bighole")
		f, err := os.Create(p)
		if err != nil {
			t.Fatalf("create sparse: %v", err)
		}
		if _, err := f.Write([]byte("head")); err != nil {
			t.Fatalf("write sparse: %v", err)
		}
		if _, err := f.Seek(64<<20, io.SeekStart); err != nil {
			t.Fatalf("seek sparse: %v", err)
		}
		if _, err := f.Write([]byte("tail")); err != nil {
			t.Fatalf("write sparse tail: %v", err)
		}
		f.Close()
	}
	if feats&(FeatACLs|FeatCaps) != 0 {
		// ACLs and capabilities are xattrs in the system/security namespace.
		mkd("acls")
		write("acls/file.txt", []byte("acl"))
		// FeatACLs: user ACL entries via system.posix_acl_access
		if feats&FeatACLs != 0 {
			setXattr(t, filepath.Join(dir, "acls/file.txt"), "system.posix_acl_access",
				[]byte{0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x06, 0x00, 0xff, 0xff, 0xff, 0xff,
					0x02, 0x00, 0x04, 0x00, 0xff, 0xff, 0xff, 0xff, 0x20, 0x00, 0x00, 0x00,
					0x04, 0x00, 0x20, 0x00, 0xff, 0xff, 0xff, 0xff, 0x10, 0x00, 0x00, 0x00})
		}
		// FeatCaps: security.capability
		if feats&FeatCaps != 0 {
			setXattr(t, filepath.Join(dir, "acls/file.txt"), "security.capability",
				[]byte{0x01, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		}
	}
	if feats&(FeatDevices|FeatFifos) != 0 {
		mkd("special")
		if feats&FeatDevices != 0 {
			if err := unix.Mknod(filepath.Join(dir, "special/chardev"), unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3))); err != nil {
				if os.Geteuid() != 0 {
					t.Logf("skip devices: mknod: %v", err)
				} else {
					t.Fatalf("mknod chardev: %v", err)
				}
			} else {
				m.Devices = append(m.Devices, "special/chardev")
			}
			if err := unix.Mknod(filepath.Join(dir, "special/blockdev"), unix.S_IFBLK|0o600, int(unix.Mkdev(7, 0))); err != nil {
				if os.Geteuid() != 0 {
					t.Logf("skip devices: mknod: %v", err)
				} else {
					t.Fatalf("mknod blockdev: %v", err)
				}
			} else {
				m.Devices = append(m.Devices, "special/blockdev")
			}
		}
		if feats&FeatFifos != 0 {
			if err := unix.Mkfifo(filepath.Join(dir, "special/fifo"), 0o600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
			m.Fifos = append(m.Fifos, "special/fifo")
		}
	}
	if feats&FeatOwnership != 0 {
		if os.Geteuid() != 0 {
			t.Log("skip ownership wiring: requires root")
		}
	}

	sort.Strings(m.Files)
	sort.Strings(m.Dirs)
	sort.Strings(m.Symlinks)
	return m
}

// MapPath maps a manifest-relative path into dir.
func MapPath(dir, rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
