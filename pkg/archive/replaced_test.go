//go:build unix

package archive

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func mustMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s: mode %o, want %o — a file the restore never wrote was changed", path, got, want)
	}
}

// TestReplacedDirectoryFixDoesNotFollowItsReplacement is the regression for
// the final directory pass following a symlink that later replaced the
// directory: "l/" 0777 then "l -> victim" made a file already in the
// destination world-writable.
func TestReplacedDirectoryFixDoesNotFollowItsReplacement(t *testing.T) {
	dst := t.TempDir()
	victim := filepath.Join(dst, "victim")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := tarWith(t,
		tar.Header{Name: "l/", Typeflag: tar.TypeDir, Mode: 0o777},
		tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "victim", Mode: 0o777},
	)
	if _, err := extractorFor(ExtractOptions{Overwrite: true}).Extract(context.Background(), buf, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	mustMode(t, victim, 0o600)
}

// TestReplacedAncestorDropsTheDirectoryFix: the same through a parent. Once
// "p" is a symlink to a directory that was already there, the fix recorded
// for "p/q" resolves inside it.
func TestReplacedAncestorDropsTheDirectoryFix(t *testing.T) {
	dst := t.TempDir()
	existing := filepath.Join(dst, "other", "q")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	buf := tarWith(t,
		tar.Header{Name: "p/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "p/q/", Typeflag: tar.TypeDir, Mode: 0o777},
		tar.Header{Name: "p", Typeflag: tar.TypeSymlink, Linkname: "other", Mode: 0o777},
	)
	if _, err := extractorFor(ExtractOptions{Overwrite: true}).Extract(context.Background(), buf, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	mustMode(t, existing, 0o700)
}

// TestRecreatedDirectoryStillGetsItsMode: dropping stale fixes must not drop
// the fix of a directory the archive creates again after the replacement.
func TestRecreatedDirectoryStillGetsItsMode(t *testing.T) {
	dst := t.TempDir()
	buf := tarWith(t,
		tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o700},
		tar.Header{Name: "d", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o751},
	)
	if _, err := extractorFor(ExtractOptions{Overwrite: true}).Extract(context.Background(), buf, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	mustMode(t, filepath.Join(dst, "d"), 0o751)
}

// TestHardlinkToARemovedSubtreeIsRefused is the regression for the stale
// hardlink targets: replacing "d" removed d/secret from disk but not from the
// set of files this run wrote, so "leak" was linked to e/secret — a file
// already in the destination — through the new symlink.
func TestHardlinkToARemovedSubtreeIsRefused(t *testing.T) {
	dst := t.TempDir()
	if err := os.Mkdir(filepath.Join(dst, "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "e", "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := tarWith(t,
		tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "d/secret", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6},
		tar.Header{Name: "d", Typeflag: tar.TypeSymlink, Linkname: "e", Mode: 0o777},
		tar.Header{Name: "leak", Typeflag: tar.TypeLink, Linkname: "d/secret", Mode: 0o666},
	)
	stats, _ := extractorFor(ExtractOptions{Overwrite: true}).Extract(context.Background(), buf, dst)
	if _, err := os.Lstat(filepath.Join(dst, "leak")); err == nil {
		t.Fatal("hardlink created to a file this restore did not write")
	}
	if stats.Skipped == 0 {
		t.Fatal("the refused hardlink was not reported")
	}
}
