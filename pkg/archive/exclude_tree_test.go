package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cacheTree builds root/a/.cache/{secret,sub/deep} and root/a/keep/f.
func cacheTree(t *testing.T) (root string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "root")
	for _, d := range []string{"a/.cache/sub", "a/keep"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a/.cache/secret", "a/.cache/sub/deep", "a/keep/f"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestExcludeGlobDropsTheWholeSubtree is the regression for an exclusion
// spelled with a glob: only the directory entry matched, so its content was
// archived without it — orphan entries holding exactly the data the operator
// asked to leave out.
func TestExcludeGlobDropsTheWholeSubtree(t *testing.T) {
	for _, pat := range []string{"**/.cache", "*/a/.cache", "*/*/.cache", "root/a/.cache"} {
		t.Run(pat, func(t *testing.T) {
			root := cacheTree(t)
			paths, _ := excludePaths(t, root, Options{Strict: true, Excludes: []string{pat}})
			for _, p := range paths {
				if strings.Contains(p, ".cache") {
					t.Errorf("excluded subtree archived: %q (all: %v)", p, paths)
				}
			}
			if !contains(paths, "root/a/keep/f") {
				t.Fatalf("the exclusion took unrelated paths: %v", paths)
			}
		})
	}
}

// TestExcludedSubtreeIsNeverOpened: an exclusion is also how an operator keeps
// the backup away from data it must not touch. An unreadable file below an
// excluded directory fails a strict backup only if something opens it.
func TestExcludedSubtreeIsNeverOpened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	root := cacheTree(t)
	locked := filepath.Join(root, "a", ".cache", "sub", "deep")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	opts := Options{Strict: true, Excludes: []string{"**/.cache"}}
	paths, _ := excludePaths(t, root, opts)
	if contains(paths, "root/a/.cache/sub/deep") {
		t.Fatalf("excluded file archived: %v", paths)
	}
	caps, err := PreflightBackup(context.Background(), []string{root}, opts.Excludes)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c.Name == "read-all-files" && !c.Available {
			t.Fatalf("preflight counted an excluded file as unreadable: %s", c.Reason)
		}
	}
}

// TestEstimateAgreesWithTheArchiveOnExcludedSubtrees: the estimate sizes the
// backup and its temp space, so it must count what the archive will hold and
// nothing below an excluded directory.
func TestEstimateAgreesWithTheArchiveOnExcludedSubtrees(t *testing.T) {
	root := cacheTree(t)
	opts := Options{Strict: true, Excludes: []string{"**/.cache"}}
	tot, err := Estimate(context.Background(), []string{root}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if tot.Files != 1 || tot.Bytes != int64(len("a/keep/f")) {
		t.Fatalf("estimate = %d files, %d bytes; want 1 file, %d bytes", tot.Files, tot.Bytes, len("a/keep/f"))
	}
}

// TestSelectsUsesTheTreeRule: the extract-side filter is the third copy of
// the rule and must agree with the other two.
func TestSelectsUsesTheTreeRule(t *testing.T) {
	if selects(ExtractOptions{Excludes: []string{"root/a"}}, "root/a/f") {
		t.Fatal("a bare excluded directory kept its content")
	}
	if !selects(ExtractOptions{Includes: []string{"**/keep"}}, "root/a/keep/f") {
		t.Fatal("an included directory dropped its content")
	}
	if selects(ExtractOptions{Includes: []string{"**/keep"}}, "root/a/other") {
		t.Fatal("an include selected an unrelated path")
	}
}

func excludePaths(t *testing.T, root string, opts Options) ([]string, Stats) {
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
	entries := w.Entries()
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return paths, stats
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
