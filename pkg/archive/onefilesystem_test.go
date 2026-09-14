package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mountBoundary makes deviceOf report a different device for every entry whose
// name carries mountMarker, which is what the kernel reports once something is
// mounted there. Nothing else about the walk changes.
//
// os.FileInfo carries no path, so the boundary has to travel in the name: the
// fixtures below name the mount point and everything under it with a prefix no
// other entry uses.
func mountBoundary(t *testing.T) {
	t.Helper()
	real := deviceOf
	t.Cleanup(func() { deviceOf = real })
	deviceOf = func(fi os.FileInfo) (uint64, bool) {
		if strings.HasPrefix(fi.Name(), mountMarker) {
			return 99, true
		}
		return 1, true
	}
}

const mountMarker = "mnt-"

// oneFileSystemTree builds a tree whose "mnt-point" directory stands for a
// mount point, with a file and a subdirectory below it.
func oneFileSystemTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	for _, dir := range []string{"sub", mountMarker + "point", filepath.Join(mountMarker+"point", mountMarker+"deeper")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join("sub", "stays.txt"):                                                "stesso filesystem",
		filepath.Join(mountMarker+"point", mountMarker+"beyond.txt"):                     "oltre il mount",
		filepath.Join(mountMarker+"point", mountMarker+"deeper", mountMarker+"deep.txt"): "molto oltre",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func archivePaths(t *testing.T, root string, opts Options) ([]string, Stats) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf, opts)
	if err := w.AddRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return entryPaths(w.Entries()), stats
}

// TestOneFileSystemKeepsTheMountPointDirectory: the option says do not cross a
// mount point, not "pretend it is not there". `tar --one-file-system` and
// `rsync -x` both archive the directory and skip what is below it; dropping it
// too restored a tree with nowhere to mount anything back — a /srv/data
// without the /srv/data/db to remount into.
func TestOneFileSystemKeepsTheMountPointDirectory(t *testing.T) {
	root := oneFileSystemTree(t)
	mountBoundary(t)

	got, stats := archivePaths(t, root, Options{OneFileSystem: true, PreserveXattrs: true})
	want := []string{"tree", "tree/" + mountMarker + "point", "tree/sub", "tree/sub/stays.txt"}
	if len(got) != len(want) {
		t.Fatalf("archived %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("archived %v, want %v", got, want)
		}
	}
	// One boundary not descended into. The entries below it are never
	// enumerated — that is the point of the option — so one is the only
	// number that can be reported.
	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (one mount point)", stats.Skipped)
	}
	if stats.Dirs != 3 {
		t.Errorf("Dirs = %d, want 3 (root, the mount point, sub)", stats.Dirs)
	}
}

// TestWithoutOneFileSystemTheWholeTreeIsArchived is the other half: without
// the option the boundary means nothing, so the test above measures the option
// and not the fixture.
func TestWithoutOneFileSystemTheWholeTreeIsArchived(t *testing.T) {
	root := oneFileSystemTree(t)
	mountBoundary(t)

	got, stats := archivePaths(t, root, Options{PreserveXattrs: true})
	if len(got) != 7 {
		t.Fatalf("archived %d entries, want 7: %v", len(got), got)
	}
	if stats.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", stats.Skipped)
	}
	beyond := "tree/" + mountMarker + "point/" + mountMarker + "beyond.txt"
	found := false
	for _, p := range got {
		if p == beyond {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s missing without --one-file-system: %v", beyond, got)
	}
}

// TestOneFileSystemLeavesOutANonDirectoryOnAnotherDevice: a bind-mounted file
// has no shape to preserve, so it is left out rather than archived empty.
func TestOneFileSystemLeavesOutANonDirectoryOnAnotherDevice(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stays.txt"), []byte("qui"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, mountMarker+"bound.txt"), []byte("altrove"), 0o644); err != nil {
		t.Fatal(err)
	}
	mountBoundary(t)

	got, stats := archivePaths(t, root, Options{OneFileSystem: true, PreserveXattrs: true})
	for _, p := range got {
		if strings.Contains(p, mountMarker) {
			t.Fatalf("a file on another device was archived: %v", got)
		}
	}
	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", stats.Skipped)
	}
}
