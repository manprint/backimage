//go:build unix

package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadMetaBasic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	mt := time.Unix(1700000000, 123456789)
	os.WriteFile(p, []byte("x"), 0o644)
	os.Chtimes(p, mt, mt)
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	e := &Entry{}
	if err := readMeta(p, fi, Options{}, e); err != nil {
		t.Fatal(err)
	}
	if e.UID != os.Getuid() || e.GID != os.Getgid() {
		t.Errorf("uid/gid = %d/%d, want %d/%d", e.UID, e.GID, os.Getuid(), os.Getgid())
	}
	if e.ModTime.UnixNano() != mt.UnixNano() {
		t.Errorf("mtime = %v, want %v", e.ModTime, mt)
	}
	if e.Mode != 0 {
		t.Errorf("readMeta must not set Mode (writer does), got %v", e.Mode)
	}
}

func TestReadMetaNumericOwner(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	os.WriteFile(p, []byte("x"), 0o644)
	fi, _ := os.Lstat(p)
	e := &Entry{}
	if err := readMeta(p, fi, Options{NumericOwner: true}, e); err != nil {
		t.Fatal(err)
	}
	if e.Uname != "" || e.Gname != "" {
		t.Errorf("NumericOwner must skip name resolution, got %q/%q", e.Uname, e.Gname)
	}
}

func TestReadMetaTimesEpochAndFuture(t *testing.T) {
	dir := t.TempDir()
	epoch := time.Unix(0, 123456789)
	future := time.Unix(2200*365*24*3600, 987654321)
	for _, tt := range []struct {
		name string
		t    time.Time
	}{
		{"epoch", epoch},
		{"future", future},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(dir, tt.name)
			os.WriteFile(p, []byte("x"), 0o644)
			os.Chtimes(p, tt.t, tt.t)
			fi, _ := os.Lstat(p)
			if fi.ModTime().Unix() != tt.t.Unix() {
				t.Skipf("filesystem cannot represent %v (got %v)", tt.t, fi.ModTime())
			}
			e := &Entry{}
			if err := readMeta(p, fi, Options{}, e); err != nil {
				t.Fatal(err)
			}
			if e.ModTime.UnixNano() != tt.t.UnixNano() {
				t.Errorf("mtime = %v, want %v", e.ModTime, tt.t)
			}
		})
	}
}

func TestReadXattrsVariants(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Setxattr(dir, "user.backimage-probe", []byte{1}, 0); err != nil {
		t.Skipf("xattrs not supported here: %v", err)
	}
	unix.Removexattr(dir, "user.backimage-probe")

	p := filepath.Join(dir, "f")
	os.WriteFile(p, []byte("x"), 0o644)
	empty := []byte{}
	binary := []byte{0x00, 0x01, 0x00, 0xff, 0x0a, 0x00}
	big := make([]byte, 3500)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := unix.Setxattr(p, "user.empty", empty, 0); err != nil {
		t.Fatalf("setxattr empty: %v", err)
	}
	if err := unix.Setxattr(p, "user.binary", binary, 0); err != nil {
		t.Fatalf("setxattr binary: %v", err)
	}
	if err := unix.Setxattr(p, "user.big", big, 0); err != nil {
		t.Skipf("cannot set 3500B xattr here (ea_inode?): %v", err)
	}
	fi, _ := os.Lstat(p)
	e := &Entry{}
	if err := readMeta(p, fi, Options{PreserveXattrs: true}, e); err != nil {
		t.Fatal(err)
	}
	if len(e.Xattrs) != 3 {
		t.Fatalf("xattrs = %d, want 3: %v", len(e.Xattrs), e.Xattrs)
	}
	if got := e.Xattrs["user.empty"]; len(got) != 0 {
		t.Errorf("user.empty = %q, want empty", got)
	}
	if string(e.Xattrs["user.binary"]) != string(binary) {
		t.Errorf("user.binary corrupted: %q", e.Xattrs["user.binary"])
	}
	if string(e.Xattrs["user.big"]) != string(big) {
		t.Errorf("user.big corrupted: %d bytes", len(e.Xattrs["user.big"]))
	}
}

func TestReadXattrsERANGE(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Setxattr(dir, "user.backimage-probe", []byte{1}, 0); err != nil {
		t.Skipf("xattrs not supported here: %v", err)
	}
	unix.Removexattr(dir, "user.backimage-probe")

	p := filepath.Join(dir, "f")
	os.WriteFile(p, []byte("x"), 0o644)
	// First a small read, then grow: forces a re-allocated listing in the
	// same process. Tolerant: only the final value is verified.
	if err := unix.Setxattr(p, "user.small", []byte("s"), 0); err != nil {
		t.Skipf("xattr unsupported: %v", err)
	}
	if _, err := readXattrs(p); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 3000)
	for i := range big {
		big[i] = byte(i)
	}
	if err := unix.Setxattr(p, "user.big", big, 0); err != nil {
		t.Skipf("cannot grow xattr: %v", err)
	}
	xs, err := readXattrs(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(xs["user.big"]) != string(big) {
		t.Errorf("user.big corrupted after growth")
	}
}

func TestReadXattrsSymlinkDoesNotInherit(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Setxattr(dir, "user.backimage-probe", []byte{1}, 0); err != nil {
		t.Skipf("xattrs not supported here: %v", err)
	}
	unix.Removexattr(dir, "user.backimage-probe")

	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("x"), 0o644)
	if err := unix.Setxattr(target, "user.mark", []byte("on-target"), 0); err != nil {
		t.Skipf("xattr unsupported: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(link)
	e := &Entry{}
	if err := readMeta(link, fi, Options{PreserveXattrs: true}, e); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Xattrs["user.mark"]; ok {
		t.Errorf("symlink entry inherited target xattr user.mark")
	}
}

func TestReadOneXattrMissingName(t *testing.T) {
	dir := t.TempDir()
	_, err := readOneXattr(filepath.Join(dir, "nonexistent-file"), "user.absent")
	if err == nil {
		t.Fatal("Lgetxattr on missing file must error")
	}
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	val, err := readOneXattr(f, "user.absent")
	if err != nil {
		t.Fatalf("ENODATA must return nil, nil: %v", err)
	}
	if val != nil {
		t.Fatal("want nil value")
	}
	e := &Entry{}
	fake := fakeFileInfo{}
	if err := readMeta(dir, fake, Options{}, e); err == nil {
		t.Fatal("readMeta with non-Stat_t fi must error")
	}
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "x" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return "not a Stat_t" }
