//go:build windows

package archive

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func extractOnWindows(t *testing.T, dst string, opts ExtractOptions, entries ...tar.Header) (Stats, error) {
	t.Helper()
	return extractorFor(opts).Extract(context.Background(), tarWith(t, entries...), dst)
}

// The Windows extractor ignored --include, --exclude and --strip-components
// entirely: a selective restore wrote the whole backup, silently, and with
// --overwrite it wrote it over the destination. Selection is not a property of
// the operating system.
func TestWindowsSelectiveRestoreExtractsOnlyWhatWasAsked(t *testing.T) {
	dst := t.TempDir()
	stats, err := extractOnWindows(t, dst, ExtractOptions{Includes: []string{"**/wanted.txt"}},
		tar.Header{Name: "tree/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "tree/wanted.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
		tar.Header{Name: "tree/other.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "tree", "wanted.txt")); err != nil {
		t.Fatalf("the selected file is missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "tree", "other.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file outside the selection was restored: %v", err)
	}
	if stats.Files != 1 {
		t.Fatalf("files = %d, want 1", stats.Files)
	}
}

func TestWindowsStripComponentsApplies(t *testing.T) {
	dst := t.TempDir()
	if _, err := extractOnWindows(t, dst, ExtractOptions{StripComponents: 1},
		tar.Header{Name: "tree/sub/a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "sub", "a.txt")); err != nil {
		t.Fatalf("--strip-components was ignored: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "tree")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stripped component was recreated")
	}
}

// A device, a fifo or anything else Windows cannot hold used to fall into the
// default branch and be created as an empty regular file — no error, no count,
// and a file that looks restored.
func TestWindowsUnsupportedTypesAreReportedNotFaked(t *testing.T) {
	dst := t.TempDir()
	stats, err := extractOnWindows(t, dst, ExtractOptions{},
		tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3},
		tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0o644},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, name := range []string{filepath.Join("dev", "null"), "pipe"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%q was materialised: %v", name, err)
		}
	}
	if stats.Skipped != 2 || len(stats.Errors) != 2 {
		t.Fatalf("stats = %+v, want two reported skips", stats)
	}
	for _, e := range stats.Errors {
		if !errors.Is(e, errUnsupportedEntry) {
			t.Fatalf("error %v is not classified as an unsupported entry", e)
		}
	}
}

// A name Windows cannot represent is skipped and reported, not silently
// rewritten into a different name or a directory level.
func TestWindowsUnrepresentableNamesAreSkipped(t *testing.T) {
	dst := t.TempDir()
	stats, err := extractOnWindows(t, dst, ExtractOptions{},
		tar.Header{Name: "back\\slash.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
		tar.Header{Name: "colon:name.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
		tar.Header{Name: "ok.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if stats.Files != 1 || stats.Skipped != 2 {
		t.Fatalf("stats = %+v, want one file written and two names skipped", stats)
	}
	if _, err := os.Lstat(filepath.Join(dst, "back")); err == nil {
		t.Fatal("a backslash became a directory level")
	}
	for _, e := range stats.Errors {
		if !errors.Is(e, errUnsupportedName) {
			t.Fatalf("error %v is not classified as an unrepresentable name", e)
		}
	}
}

// Same rule as Unix: a hardlink may only point at a regular file this run has
// already written.
func TestWindowsHardlinkNeedsARestoredFirstName(t *testing.T) {
	dst := t.TempDir()
	stats, err := extractOnWindows(t, dst, ExtractOptions{},
		tar.Header{Name: "orig", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		tar.Header{Name: "copy", Typeflag: tar.TypeLink, Linkname: "orig", Mode: 0o644},
		tar.Header{Name: "orphan", Typeflag: tar.TypeLink, Linkname: "absent", Mode: 0o644},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "copy")); err != nil {
		t.Fatalf("the legitimate hardlink is missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "orphan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a hardlink to a name never restored was materialised")
	}
	if stats.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", stats.Skipped)
	}
}

// The destination holds a symlink pointing outside it; nothing may be written
// through it.
func TestWindowsIntermediateLinkOutOfTheDestinationIsRefused(t *testing.T) {
	outside := t.TempDir()
	dst := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "sub")); err != nil {
		t.Skipf("symlinks are not available on this host: %v", err)
	}
	stats, err := extractOnWindows(t, dst, ExtractOptions{Overwrite: true},
		tar.Header{Name: "sub/planted.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "planted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was written outside the destination: %v", err)
	}
	if stats.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", stats.Skipped)
	}
}
