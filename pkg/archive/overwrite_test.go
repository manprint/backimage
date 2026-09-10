//go:build unix

package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// tarWith builds an archive from a list of headers with optional bodies.
func tarWith(t *testing.T, entries ...tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := range entries {
		h := entries[i]
		body := make([]byte, h.Size)
		for j := range body {
			body[j] = 'x'
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// --overwrite used to mean "replace the destination tree": an existing
// directory was passed to RemoveAll before the archived one was created, so
// every child the backup did not contain went with it. Restoring one selected
// subtree into a populated directory silently deleted its siblings.
func TestOverwriteDoesNotDeleteChildrenTheBackupDoesNotContain(t *testing.T) {
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "keep", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	stranger := filepath.Join(dst, "keep", "stranger.txt")
	if err := os.WriteFile(stranger, []byte("not in the backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deepStranger := filepath.Join(dst, "keep", "nested", "deep.txt")
	if err := os.WriteFile(deepStranger, []byte("also not in the backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	arc := tarWith(t,
		tar.Header{Name: "keep", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "keep/restored.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
	)
	x := NewExtractor(ExtractOptions{Overwrite: true})
	if _, err := x.Extract(context.Background(), arc, dst); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{stranger, deepStranger} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("--overwrite deleted %s, which the backup never contained: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "keep", "restored.txt")); err != nil {
		t.Fatalf("the archived file was not restored: %v", err)
	}
}

// The removal is still required when the kinds disagree: a directory cannot
// become a file, or a file a symlink, by writing over it.
func TestOverwriteReplacesObjectsOfADifferentKind(t *testing.T) {
	dst := t.TempDir()
	// A directory where the archive has a regular file.
	if err := os.MkdirAll(filepath.Join(dst, "becomes-file", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file where the archive has a directory.
	if err := os.WriteFile(filepath.Join(dst, "becomes-dir"), []byte("file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink where the archive has a regular file.
	if err := os.Symlink("/dev/null", filepath.Join(dst, "becomes-regular")); err != nil {
		t.Fatal(err)
	}

	arc := tarWith(t,
		tar.Header{Name: "becomes-file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
		tar.Header{Name: "becomes-dir", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "becomes-regular", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	)
	x := NewExtractor(ExtractOptions{Overwrite: true})
	if _, err := x.Extract(context.Background(), arc, dst); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(dst, "becomes-file"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("a directory must be replaced by the archived regular file: %v %v", fi, err)
	}
	fi, err = os.Lstat(filepath.Join(dst, "becomes-dir"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("a regular file must be replaced by the archived directory: %v %v", fi, err)
	}
	fi, err = os.Lstat(filepath.Join(dst, "becomes-regular"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("a symlink must be replaced by the archived regular file: %v %v", fi, err)
	}
}

// A regular file replacing a shorter regular file must not keep the tail of
// the old content.
func TestOverwriteTruncatesAnExistingFile(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "f.txt"), bytes.Repeat([]byte("A"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	arc := tarWith(t, tar.Header{Name: "f.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	x := NewExtractor(ExtractOptions{Overwrite: true})
	if _, err := x.Extract(context.Background(), arc, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "xxx" {
		t.Fatalf("the previous content was not replaced: %d bytes %q", len(got), got[:min(len(got), 16)])
	}
}

// The same name twice in one archive is the degenerate case of the rule: the
// second entry replaces the first, and a repeated directory is not a reason to
// delete what the first pass just wrote under it.
func TestOverwriteHandlesRepeatedNamesInOneArchive(t *testing.T) {
	dst := t.TempDir()
	arc := tarWith(t,
		tar.Header{Name: "d", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "d/first.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		tar.Header{Name: "d", Typeflag: tar.TypeDir, Mode: 0o750},
		tar.Header{Name: "d/second.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		tar.Header{Name: "dup.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
		tar.Header{Name: "dup.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
	)
	x := NewExtractor(ExtractOptions{Overwrite: true})
	if _, err := x.Extract(context.Background(), arc, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"d/first.txt", "d/second.txt"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); err != nil {
			t.Fatalf("a repeated directory entry dropped %s: %v", name, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dst, "dup.txt"))
	if err != nil || len(got) != 2 {
		t.Fatalf("the last entry with a name wins: %q %v", got, err)
	}
}

// Without --overwrite nothing changes: an existing name is still an error.
func TestWithoutOverwriteAnExistingNameStillFails(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "f.txt"), []byte("here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	arc := tarWith(t, tar.Header{Name: "f.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	x := NewExtractor(ExtractOptions{Strict: true})
	if _, err := x.Extract(context.Background(), arc, dst); err == nil {
		t.Fatal("without --overwrite an existing name must still be refused")
	}
}
