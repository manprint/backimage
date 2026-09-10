package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// --continue used to cancel the filters. `restoreExtract` was told the stream
// was already filtered and therefore dropped --include/--exclude, while the
// stream it received came from the tolerant path, which had no notion of a
// selection: the whole backup was extracted, and with --overwrite it was
// written over the destination.

func tarEntryNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the produced tar must be readable: %v", err)
		}
		names = append(names, h.Name)
	}
	return names
}

func TestContinueKeepsExcludesWhenExtracting(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	dest := t.TempDir()
	if _, _, err := runRoot(t, "restore", "example.test/repo:tag", "-x", "-C", dest,
		"--continue", "--exclude", "**/file.txt", "--overwrite"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "root", "file.txt")); err == nil {
		t.Fatal("--continue must not cancel --exclude: the excluded file was written to disk")
	}
	if _, err := os.Lstat(filepath.Join(dest, "root")); err != nil {
		t.Fatalf("the selected parent directory must still be restored: %v", err)
	}
}

func TestContinueKeepsExcludesWhenWritingATar(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	out := filepath.Join(t.TempDir(), "out.tar")
	if _, _, err := runRoot(t, "restore", "example.test/repo:tag", "-o", out,
		"--continue", "--exclude", "**/file.txt"); err != nil {
		t.Fatal(err)
	}
	for _, name := range tarEntryNames(t, out) {
		if name == "root/file.txt" {
			t.Fatal("--continue must not cancel --exclude on the tar output either")
		}
	}
}

func TestContinueKeepsIncludesWhenWritingATar(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	out := filepath.Join(t.TempDir(), "out.tar")
	if _, _, err := runRoot(t, "restore", "example.test/repo:tag", "-o", out,
		"--continue", "--include", "**/file.txt"); err != nil {
		t.Fatal(err)
	}
	names := tarEntryNames(t, out)
	var sawFile bool
	for _, name := range names {
		if name == "root/file.txt" {
			sawFile = true
		}
	}
	if !sawFile {
		t.Fatalf("the included entry must be present: %v", names)
	}
}

func TestContinueAppliesStripComponentsWhenExtracting(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	dest := t.TempDir()
	if _, _, err := runRoot(t, "restore", "example.test/repo:tag", "-x", "-C", dest,
		"--continue", "--include", "**/file.txt", "--strip-components", "1", "--overwrite"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "file.txt")); err != nil {
		t.Fatalf("--strip-components must still apply with --continue: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "root", "file.txt")); err == nil {
		t.Fatal("--strip-components was ignored: the unstripped path exists")
	}
}

// Without filters --continue keeps behaving as before: everything that
// verifies is written out.
func TestContinueWithoutFiltersStillRestoresEverything(t *testing.T) {
	s, tarBytes := newMockImageSource(t, false)
	withMockSource(t, s)
	out := filepath.Join(t.TempDir(), "out.tar")
	if _, _, err := runRoot(t, "restore", "example.test/repo:tag", "-o", out, "--continue"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, tarBytes) {
		t.Fatal("--continue on a healthy backup must reproduce the whole tar")
	}
}
