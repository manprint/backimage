package fixtures

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCompareCpA proves the comparator is not blind: an exact copy (cp -a,
// which preserves mode/times/owners/xattrs/hardlinks/symlinks) must compare
// clean, with nanosecond timestamps and hostile names included.
func TestCompareCpA(t *testing.T) {
	if _, err := exec.LookPath("cp"); err != nil {
		t.Skip("cp not available")
	}
	root := t.TempDir()
	want := filepath.Join(root, "want")
	got := filepath.Join(root, "got")
	feats := FeatBasic | FeatPerms | FeatSymlinks | FeatHardlinks | FeatXattrs | FeatNames | FeatTimes
	Build(t, want, feats)
	t.Log("chmod secret000 -> 0400 so non-root cp can read it")
	if err := os.Chmod(filepath.Join(want, "perms/secret000"), 0o400); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("cp", "-a", want, got).CombinedOutput()
	if err != nil {
		t.Fatalf("cp -a: %v\n%s", err, out)
	}
	if diffs := CompareTrees(t, want, got, CompareOptions{}); len(diffs) != 0 {
		t.Fatalf("cp -a copy must be identical, got %d diffs:\n%+v", len(diffs), diffs)
	}
}

// TestCompareCpR proves the comparator is not blind: cp -r drops metadata,
// so it must report plenty of differences.
func TestCompareCpR(t *testing.T) {
	if _, err := exec.LookPath("cp"); err != nil {
		t.Skip("cp not available")
	}
	root := t.TempDir()
	want := filepath.Join(root, "want")
	got := filepath.Join(root, "got")
	feats := FeatBasic | FeatPerms | FeatSymlinks | FeatHardlinks | FeatXattrs | FeatNames | FeatTimes
	Build(t, want, feats)
	if err := os.Chmod(filepath.Join(want, "perms/secret000"), 0o400); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("cp", "-r", want, got).CombinedOutput()
	if err != nil {
		t.Fatalf("cp -r: %v\n%s", err, out)
	}
	diffs := CompareTrees(t, want, got, CompareOptions{})
	if len(diffs) < 10 {
		t.Fatalf("cp -r must lose metadata, expected >=10 diffs, got %d:\n%+v", len(diffs), diffs)
	}
}

func TestRequiresRoot(t *testing.T) {
	got := RequiresRoot(FeatACLs | FeatCaps | FeatDevices | FeatOwnership | FeatBasic)
	want := FeatACLs | FeatCaps | FeatDevices | FeatOwnership
	if got != want {
		t.Fatalf("RequiresRoot = %v, want %v", got, want)
	}
}
