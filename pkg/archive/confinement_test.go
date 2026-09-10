//go:build unix

package archive

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func extractTo(t *testing.T, dst string, opts ExtractOptions, entries ...tar.Header) (Stats, error) {
	t.Helper()
	buf := tarWith(t, entries...)
	return extractorFor(opts).Extract(context.Background(), buf, dst)
}

// The archive names a directory and then a symlink with the same name. Before
// the descriptor-anchored walk, the directory metadata pass ran at the end on
// the *pathname* it had recorded, by then a symlink, and chmod followed it out
// of the destination: 0700 outside became 0777, with no privileges and no race
// between processes.
func TestDirectoryReplacedBySymlinkDoesNotLeakTheFinalChmod(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()

	stats, err := extractTo(t, dst, ExtractOptions{Overwrite: true},
		tar.Header{Name: "pivot/", Typeflag: tar.TypeDir, Mode: 0o777},
		tar.Header{Name: "pivot", Typeflag: tar.TypeSymlink, Linkname: victim, Mode: 0o777},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	fi, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("the mode of a directory outside the destination changed to %o", fi.Mode().Perm())
	}
	// The refusal is a degradation, not a failure: the entry that could not be
	// finalised is reported rather than silently dropped.
	if stats.Degraded["mode"] == 0 {
		t.Fatalf("the refused chmod was not reported: %+v", stats.Degraded)
	}
}

// A pre-existing symlink used as an intermediate component points outside; the
// entry below it must not be written through it.
func TestIntermediateSymlinkOutOfTheDestinationIsRefused(t *testing.T) {
	outside := t.TempDir()
	dst := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "sub")); err != nil {
		t.Fatal(err)
	}
	stats, err := extractTo(t, dst, ExtractOptions{Overwrite: true},
		tar.Header{Name: "sub/planted.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "planted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was written outside the destination: %v", err)
	}
	if stats.Skipped != 1 || len(stats.Errors) != 1 {
		t.Fatalf("the refused entry was not reported: skipped=%d errors=%v", stats.Skipped, stats.Errors)
	}
}

// hdr.Linkname never went through the checks applied to hdr.Name: it was
// joined to the destination and linked as-is. A link to a file outside gave
// the restore a second name for it, and the metadata pass then rewrote that
// file's mode through the shared inode.
func TestHardlinkOutOfTheDestinationIsRefused(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()

	stats, err := extractTo(t, dst, ExtractOptions{Overwrite: true},
		tar.Header{Name: "stolen", Typeflag: tar.TypeLink, Linkname: "../" +
			filepath.Base(outside) + "/secret", Mode: 0o644},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "stolen")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the hardlink was created: %v", err)
	}
	fi, err := os.Stat(secret)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("the mode of a file outside the destination changed to %o", fi.Mode().Perm())
	}
	if stats.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", stats.Skipped)
	}
	if len(stats.Errors) != 1 || !errors.Is(stats.Errors[0], errLinkTargetNotRestored) {
		t.Fatalf("errors = %v, want a link-target refusal", stats.Errors)
	}
}

// DA-03: a hardlink whose first name is not part of this restore is skipped
// and reported, never reconstructed by reading whatever sits at that path.
func TestHardlinkToAFilteredFirstNameIsSkippedNotCopied(t *testing.T) {
	dst := t.TempDir()
	stats, err := extractTo(t, dst, ExtractOptions{Includes: []string{"copy"}},
		tar.Header{Name: "orig", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
		tar.Header{Name: "copy", Typeflag: tar.TypeLink, Linkname: "orig", Mode: 0o644},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the filtered hardlink was materialised anyway: %v", err)
	}
	if stats.Skipped != 1 || stats.Hardlinks != 0 || stats.Files != 0 {
		t.Fatalf("stats = %+v, want one skipped entry and nothing written", stats)
	}
}

// A forward link — the first name appears after the link in the archive — is
// handled the same way: skipped and reported. The alternative, deferring the
// entry to the end of the run, would keep the whole selection in memory for a
// case backimage's own writer never produces.
func TestForwardHardlinkIsSkippedAndReported(t *testing.T) {
	dst := t.TempDir()
	stats, err := extractTo(t, dst, ExtractOptions{},
		tar.Header{Name: "later", Typeflag: tar.TypeLink, Linkname: "orig", Mode: 0o644},
		tar.Header{Name: "orig", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if stats.Skipped != 1 || stats.Files != 1 || stats.Hardlinks != 0 {
		t.Fatalf("stats = %+v, want the link skipped and the file written", stats)
	}
}

// The ordinary case must keep working: a hardlink to a name already restored
// is a real hardlink, sharing one inode.
func TestHardlinkToARestoredNameStillShares(t *testing.T) {
	dst := t.TempDir()
	stats, err := extractTo(t, dst, ExtractOptions{},
		tar.Header{Name: "orig", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
		tar.Header{Name: "second", Typeflag: tar.TypeLink, Linkname: "orig", Mode: 0o644},
		tar.Header{Name: "third", Typeflag: tar.TypeLink, Linkname: "second", Mode: 0o644},
	)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if stats.Hardlinks != 2 || stats.Files != 1 {
		t.Fatalf("stats = %+v, want one file and two hardlinks", stats)
	}
	a, err := os.Stat(filepath.Join(dst, "orig"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"second", "third"} {
		b, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(a, b) {
			t.Fatalf("%q is not the same inode as orig", name)
		}
	}
}

// An absolute name, a traversal and an empty name are refused before any
// syscall: os.Root would stop them anyway, but the archive's own hygiene is
// worth one clear error.
func TestCheckArchivePathRefusesHostileNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "/etc/passwd", "../x", "a/../../x", "a/.."} {
		if err := checkArchivePath(name); err == nil {
			t.Errorf("checkArchivePath(%q) = nil, want a refusal", name)
		}
	}
	for _, name := range []string{"a", "a/b", "a/..b", "a/b..c", "..a"} {
		if err := checkArchivePath(name); err != nil {
			t.Errorf("checkArchivePath(%q) = %v, want nil", name, err)
		}
	}
}
