//go:build unix

package archive

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestXattrsOfAReplacedEntryAreRefused: the attributes of an entry the walk
// did not open are read only if the entry is still the object the walk's
// lstat described. A rename-over between the two used to archive the
// newcomer's attributes under the walked entry's name and metadata.
func TestXattrsOfAReplacedEntryAreRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("target", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	st, err := root.Lstat("l")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(dir, "l.new")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "l.new"), filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}

	src := source{dir: root, name: "l", path: filepath.Join(dir, "l")}
	err = readMeta(src, nil, st, Options{PreserveXattrs: true}, &Entry{})
	var lost *xattrLossError
	if !errors.As(err, &lost) || !errors.Is(err, errSourceChanged) {
		t.Fatalf("readMeta on a replaced entry = %v, want an xattr loss for a replaced source", err)
	}
}

// TestXattrsOfAnUnchangedEntryAreRead guards the ordinary case: the identity
// checks must not refuse an entry nobody touched.
func TestXattrsOfAnUnchangedEntryAreRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("target", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	st, err := root.Lstat("l")
	if err != nil {
		t.Fatal(err)
	}
	src := source{dir: root, name: "l", path: filepath.Join(dir, "l")}
	if err := readMeta(src, nil, st, Options{PreserveXattrs: true}, &Entry{}); err != nil {
		t.Fatalf("readMeta: %v", err)
	}
}
