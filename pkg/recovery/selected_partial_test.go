package recovery

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/manprint/backimage/pkg/index"
)

// tarNames lists the entry names of a tar stream.
func tarNames(t *testing.T, blob []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(blob))
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

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// entryEndingWith returns the single index entry whose path ends with suffix.
func entryEndingWith(t *testing.T, idx *index.Index, suffix string) index.FileEntry {
	t.Helper()
	var found []index.FileEntry
	for _, e := range idx.Entries {
		if strings.HasSuffix(e.Path, suffix) {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one entry ending in %q, found %d", suffix, len(found))
	}
	return found[0]
}

// Tolerating damaged chunks and honouring a selection used to be mutually
// exclusive: --continue swapped the selective stream for the tolerant one,
// which had no notion of a selection and therefore emitted the whole backup.
func TestStreamSelectedTarPartialEmitsOnlyTheSelection(t *testing.T) {
	f := makeFixture(t, false, 4096)
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	idx, err := b.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := entryEndingWith(t, idx, "/a.txt")
	other := entryEndingWith(t, idx, "sub/b.txt")

	var out bytes.Buffer
	report, err := b.StreamSelectedTarPartial(context.Background(), idx, []index.FileEntry{want}, &out, true)
	if err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, out.Bytes())
	if !hasName(names, want.Path) {
		t.Fatalf("the selected entry is missing: %v", names)
	}
	if hasName(names, other.Path) {
		t.Fatalf("an entry outside the selection was emitted: %v", names)
	}
	if report.Entries == 0 {
		t.Fatal("the report must count what it wrote")
	}
}

// With a damaged chunk the selection still holds: what survives is a subset of
// what was asked for, never a superset.
func TestStreamSelectedTarPartialKeepsTheSelectionOnDamage(t *testing.T) {
	f := makeFixture(t, false, 1024)
	b0, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := b0.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b0.Close()
	want := entryEndingWith(t, idx, "/a.txt")
	other := entryEndingWith(t, idx, "sub/b.txt")

	corruptLastChunk(t, f)

	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var out bytes.Buffer
	if _, err := b.StreamSelectedTarPartial(context.Background(), idx, []index.FileEntry{want}, &out, true); err != nil {
		t.Fatal(err)
	}
	if hasName(tarNames(t, out.Bytes()), other.Path) {
		t.Fatalf("an entry outside the selection was emitted on a damaged backup")
	}
}

// corruptLastChunk flips the last byte of the stored layer.
func corruptLastChunk(t *testing.T, f fixture) {
	t.Helper()
	data, err := os.ReadFile(f.chunkPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(f.chunkPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
