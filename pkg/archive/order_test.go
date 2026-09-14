//go:build unix

package archive

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestEmissionOrderIsAlphabeticalAtEveryDepth locks the order the writer emits
// entries in.
//
// The root's own children used to come out reverse-alphabetically while every
// deeper directory came out alphabetically: the initial stack was pushed in
// sorted order and popped from the end, whereas the per-directory push was
// already reversed to compensate. Deterministic either way, but two different
// orders inside one archive — and "the first name of a hardlink group" is
// defined by this order, so it has to mean one thing.
func TestEmissionOrderIsAlphabeticalAtEveryDepth(t *testing.T) {
	src := t.TempDir()
	base := filepath.Base(src)
	mkdir(t, src, "dir")
	mkdir(t, src, "dir/nested")
	for _, rel := range []string{
		"a.txt", "b.txt", "z.txt",
		"dir/a", "dir/m", "dir/z",
		"dir/nested/a", "dir/nested/z",
	} {
		write(t, src, rel)
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: true})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		base,
		base + "/a.txt",
		base + "/b.txt",
		base + "/dir",
		base + "/dir/a",
		base + "/dir/m",
		base + "/dir/nested",
		base + "/dir/nested/a",
		base + "/dir/nested/z",
		base + "/dir/z",
		base + "/z.txt",
	}
	if got := entryPaths(w.Entries()); !reflect.DeepEqual(got, want) {
		t.Fatalf("emission order:\n got %v\nwant %v", got, want)
	}
}

// TestEmissionOrderParentBeforeChildren states the other half of the order
// contract, on a tree wide enough that a stack bug would show: a directory is
// always emitted before anything inside it, so an extractor never has to
// create a parent it has not been told about.
func TestEmissionOrderParentBeforeChildren(t *testing.T) {
	src := t.TempDir()
	for _, dir := range []string{"b", "b/b", "a", "a/c", "a/c/d"} {
		mkdir(t, src, dir)
	}
	for _, rel := range []string{"a/c/d/leaf", "a/c/x", "b/b/y", "top"} {
		write(t, src, rel)
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: true})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	for i, e := range w.Entries() {
		if parent := path.Dir(e.Path); parent != "." && parent != e.Path {
			at, ok := seen[parent]
			if !ok {
				t.Fatalf("entry %q emitted before its directory %q", e.Path, parent)
			}
			if at > i {
				t.Fatalf("entry %q at %d precedes its directory %q at %d", e.Path, i, parent, at)
			}
		}
		seen[e.Path] = i
	}

	// Siblings of one directory, in emission order, are sorted.
	siblings := map[string][]string{}
	for _, e := range w.Entries() {
		parent := path.Dir(e.Path)
		siblings[parent] = append(siblings[parent], path.Base(e.Path))
	}
	for parent, names := range siblings {
		ordered := append([]string(nil), names...)
		sort.Strings(ordered)
		if !reflect.DeepEqual(names, ordered) {
			t.Errorf("children of %q emitted as %v, want %v", parent, names, ordered)
		}
	}
}

// TestEmissionOrderIsStableAcrossRuns is the determinism the archive digest
// depends on: same tree, same bytes.
func TestEmissionOrderIsStableAcrossRuns(t *testing.T) {
	src := t.TempDir()
	mkdir(t, src, "d")
	for _, rel := range []string{"one", "two", "three", "d/x", "d/y"} {
		write(t, src, rel)
	}
	run := func() []string {
		var buf bytes.Buffer
		w := NewWriter(&buf, Options{Strict: true})
		if err := w.AddRoot(context.Background(), src); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return entryPaths(w.Entries())
	}
	if first, second := run(), run(); !reflect.DeepEqual(first, second) {
		t.Fatalf("two runs over one tree disagree:\n%v\n%v", first, second)
	}
}

func mkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(rel), 0o644); err != nil {
		t.Fatal(err)
	}
}
