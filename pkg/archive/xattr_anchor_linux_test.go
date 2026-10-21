//go:build linux

package archive

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestXattrsOfAnUnopenedEntryStayInTheWalkedDirectory is the regression for
// the one read of the walk that did not go through a checked handle: the
// attributes of an entry without a descriptor (symlink, device, fifo, file
// that could not be opened) were read by full pathname. A parent swapped for a
// symlink between the walk and that read sent it outside the backup root.
func TestXattrsOfAnUnopenedEntryStayInTheWalkedDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	walked := filepath.Join(dir, "f")
	if err := os.WriteFile(walked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(walked, "user.origin", []byte("walked"), 0); err != nil {
		t.Skipf("no user xattrs here: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	st, err := root.Lstat("f")
	if err != nil {
		t.Fatal(err)
	}

	// The swap: the walked directory moves away and its name becomes a
	// symlink to a look-alike tree outside the root.
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(outside, "f"), "user.origin", []byte("outside"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(base, "d.moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}

	e := &Entry{}
	src := source{dir: root, name: "f", path: walked}
	if err := readMeta(src, nil, st, Options{PreserveXattrs: true}, e); err != nil {
		t.Fatalf("readMeta: %v", err)
	}
	if got := string(e.Xattrs["user.origin"]); got != "walked" {
		t.Fatalf("attributes read from %q, want the walked entry's", got)
	}
}
