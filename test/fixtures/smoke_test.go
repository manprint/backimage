//go:build unix

package fixtures

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const smokeFeats = FeatBasic | FeatPerms | FeatSymlinks | FeatHardlinks | FeatXattrs | FeatNames | FeatTimes

func TestSmokeCompareIdentical(t *testing.T) {
	dir := t.TempDir()
	Build(t, dir, smokeFeats)
	diffs := CompareTrees(t, dir, dir, CompareOptions{})
	if len(diffs) != 0 {
		t.Fatalf("same tree must be identical, got %d diffs: %+v", len(diffs), diffs)
	}
}

// Two builds differ only in the timestamps the build itself leaves behind.
// Once those are copied across the trees compare clean, so every difference
// reported after the tampering belongs to the tampering: a count alone would
// be satisfied by the build's own mtimes even with a comparator blind to
// content, mode, presence or xattrs.
func TestSmokeCompareDetectsChange(t *testing.T) {
	base := t.TempDir()
	want := filepath.Join(base, "want")
	got := filepath.Join(base, "got")
	Build(t, want, smokeFeats)
	Build(t, got, smokeFeats)
	copyTimes(t, want, got)
	if diffs := CompareTrees(t, want, got, CompareOptions{}); len(diffs) != 0 {
		t.Fatalf("two builds with the same timestamps must compare clean, got %d diffs: %+v", len(diffs), diffs)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(got, "basic/hello.txt"), []byte("tampered"), 0o644)) // #nosec G306 -- fixture
	must(os.Chmod(filepath.Join(got, "perms/sticky"), 0o600))
	must(os.Chtimes(filepath.Join(got, "times/ns.txt"), time.Unix(999, 5), time.Unix(999, 5)))
	must(os.Remove(filepath.Join(got, "empty-dir")))
	must(os.Remove(filepath.Join(got, "xattr/empty")))
	setXattr(t, filepath.Join(got, "xattr/binary"), "user.extra", []byte{1})

	diffs := CompareTrees(t, want, got, CompareOptions{})
	reported := map[string]bool{}
	for _, d := range diffs {
		reported[d.Path+" "+d.Field] = true
	}
	for _, expect := range []string{
		"basic/hello.txt content",
		"perms/sticky mode",
		"times/ns.txt mtime",
		"empty-dir exists",
		"xattr/empty exists",
		"xattr/binary xattr:user.extra",
	} {
		if !reported[expect] {
			t.Errorf("tampering not reported: %s (diffs: %+v)", expect, diffs)
		}
	}
	// Writing and removing also move the mtime of the file and of the parent
	// directory; nothing outside the tampered paths may differ.
	touched := map[string]bool{
		"basic/hello.txt": true, "perms/sticky": true, "times/ns.txt": true,
		"empty-dir": true, "xattr/empty": true, "xattr": true, "xattr/binary": true,
	}
	for _, d := range diffs {
		if !touched[d.Path] {
			t.Errorf("difference on an untouched path: %+v", d)
		}
	}
}

// copyTimes gives every entry of dst the access and modification time of the
// same entry in src, without following symlinks.
func copyTimes(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		ts := unix.NsecToTimespec(fi.ModTime().UnixNano())
		return unix.UtimesNanoAt(unix.AT_FDCWD, filepath.Join(dst, rel), []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
	})
	if err != nil {
		t.Fatalf("copy times %s -> %s: %v", src, dst, err)
	}
}

// Every entry Build creates must be in its manifest: a test that iterates the
// manifest otherwise skips what was left out without noticing.
func TestManifestCoversTheTree(t *testing.T) {
	feats := smokeFeats | FeatFifos | FeatSparse
	if !testing.Short() {
		feats |= FeatBigFile
	}
	dir := t.TempDir()
	m := Build(t, dir, feats)

	listed := map[string]bool{}
	for _, group := range [][]string{m.Files, m.Dirs, m.Symlinks, m.Devices, m.Fifos} {
		for _, rel := range group {
			listed[rel] = true
		}
	}
	for _, group := range m.HardlinkGroups {
		for _, rel := range group {
			listed[rel] = true
		}
	}
	for rel := range m.SHA {
		if !listed[rel] {
			t.Errorf("hashed but not listed: %s", rel)
		}
	}
	for _, rel := range m.Files {
		if _, ok := m.SHA[rel]; !ok {
			t.Errorf("listed file without a hash: %s", rel)
		}
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if listed[rel] {
			return nil
		}
		// A parent created on the way to a listed path is implied by it.
		if d.IsDir() {
			for l := range listed {
				if strings.HasPrefix(l, rel+"/") {
					return nil
				}
			}
		}
		t.Errorf("created but not in the manifest: %s", rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if feats&FeatBigFile != 0 {
		fi, err := os.Stat(filepath.Join(dir, "big/file.bin"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != BigFileSize {
			t.Fatalf("big file is %d bytes, want %d", fi.Size(), BigFileSize)
		}
	}
}

// The big file must not be a run of equal blocks: the chunker, the codecs and
// the dedup would all see something far easier than real data.
func TestBigFileBlocksDiffer(t *testing.T) {
	a := make([]byte, 1<<20)
	b := make([]byte, 1<<20)
	bigFileBlock(a, 0)
	bigFileBlock(b, 1)
	if string(a) == string(b) {
		t.Fatal("two big-file blocks are equal")
	}
	zeros := 0
	for _, c := range a {
		if c == 0 {
			zeros++
		}
	}
	if zeros > len(a)/64 {
		t.Fatalf("big-file block has %d zero bytes out of %d: not random-looking", zeros, len(a))
	}
}

// As root FeatOwnership hands file, directory and symlink to owners nobody
// runs as, with uid and gid distinct, so a round trip that drops or swaps them
// is caught.
func TestOwnershipFeatureAssignsForeignOwners(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	dir := t.TempDir()
	m := Build(t, dir, FeatOwnership)
	want := map[string][2]uint32{
		"owner/file": {ForeignUID, ForeignGID},
		"owner/dir":  {ForeignGID, ForeignUID},
		"owner/link": {ForeignUID + 1, ForeignGID + 1},
	}
	listed := map[string]bool{}
	for _, group := range [][]string{m.Files, m.Dirs, m.Symlinks} {
		for _, rel := range group {
			listed[rel] = true
		}
	}
	for rel, id := range want {
		if !listed[rel] {
			t.Errorf("%s missing from the manifest", rel)
		}
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(dir, rel), &st); err != nil {
			t.Fatalf("lstat %s: %v", rel, err)
		}
		if st.Uid != id[0] || st.Gid != id[1] {
			t.Errorf("%s owned by %d:%d, want %d:%d", rel, st.Uid, st.Gid, id[0], id[1])
		}
	}
}
