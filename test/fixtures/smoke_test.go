package fixtures

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSmokeCompareIdentical(t *testing.T) {
	dir := t.TempDir()
	Build(t, dir, FeatBasic|FeatPerms|FeatSymlinks|FeatHardlinks|FeatXattrs|FeatNames|FeatTimes)
	diffs := CompareTrees(t, dir, dir, CompareOptions{})
	if len(diffs) != 0 {
		t.Fatalf("same tree must be identical, got %d diffs: %+v", len(diffs), diffs)
	}
}

func TestSmokeCompareDetectsChange(t *testing.T) {
	base := t.TempDir()
	Build(t, base, FeatBasic|FeatPerms|FeatSymlinks|FeatHardlinks|FeatXattrs|FeatNames|FeatTimes)
	want := filepath.Join(base, "want")
	got := filepath.Join(base, "got")
	os.RemoveAll(want)
	os.RemoveAll(got)
	Build(t, want, FeatBasic|FeatPerms|FeatSymlinks|FeatHardlinks|FeatXattrs|FeatNames|FeatTimes)
	Build(t, got, FeatBasic|FeatPerms|FeatSymlinks|FeatHardlinks|FeatXattrs|FeatNames|FeatTimes)
	// tamper got
	os.WriteFile(filepath.Join(got, "basic/hello.txt"), []byte("tampered"), 0o644)
	os.Chmod(filepath.Join(got, "perms/sticky"), 0o600)
	os.Chtimes(filepath.Join(got, "times/ns.txt"), time.Unix(999, 5), time.Unix(999, 5))
	os.Remove(filepath.Join(got, "empty-dir"))
	os.Remove(filepath.Join(got, "xattr/empty"))
	setXattr(t, filepath.Join(got, "xattr/binary"), "user.extra", []byte{1})
	diffs := CompareTrees(t, want, got, CompareOptions{})
	if len(diffs) < 6 {
		t.Fatalf("expected >=6 diffs, got %d: %+v", len(diffs), diffs)
	}
}
