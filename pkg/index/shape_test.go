package index

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/crypt"
)

func shapeEntry(path string, offset int64) FileEntry {
	return FileEntry{
		Path: path, Type: TypeRegular, Size: 1, Mode: "0644",
		MTime:  time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		SHA256: strings.Repeat("a", 64), TarOffset: offset,
	}
}

func shapeEntries() []FileEntry {
	return []FileEntry{shapeEntry("a.txt", 0), shapeEntry("b.txt", 1536), shapeEntry("c.txt", 3072)}
}

// TestAnIndexHasAShapeItsReadersAlreadyAssume states the properties every
// consumer of the index took for granted and nobody checked: one entry per
// path, offsets that grow, names a filesystem could hold.
func TestAnIndexHasAShapeItsReadersAlreadyAssume(t *testing.T) {
	if err := validateEntries(shapeEntries()); err != nil {
		t.Fatalf("an honest index must validate: %v", err)
	}

	cases := map[string]struct {
		mutate func([]FileEntry) []FileEntry
		says   string
	}{
		"a repeated path": {
			func(e []FileEntry) []FileEntry { e[2].Path = e[0].Path; return e },
			"repeats the path of entry[0]",
		},
		"offsets that stand still": {
			func(e []FileEntry) []FileEntry { e[2].TarOffset = e[1].TarOffset; return e },
			"already started at 1536",
		},
		"offsets that go backwards": {
			func(e []FileEntry) []FileEntry { e[2].TarOffset = 512; return e },
			"already started at 1536",
		},
		"a path no filesystem can hold": {
			func(e []FileEntry) []FileEntry { e[1].Path = strings.Repeat("p", MaxPathBytes+1); return e },
			"byte path",
		},
		"a link target no filesystem can hold": {
			func(e []FileEntry) []FileEntry {
				e[1].Type, e[1].LinkTarget = TypeSymlink, strings.Repeat("t", MaxPathBytes+1)
				return e
			},
			"byte target",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateEntries(tc.mutate(shapeEntries()))
			if !errors.Is(err, ErrBadSchema) {
				t.Fatalf("validateEntries = %v, want ErrBadSchema", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the refusal must say what is wrong, got %q", err)
			}
		})
	}
}

// TestTheEntryCountIsBoundedWhileItIsRead is the allocation half: the guard
// has to stop the decoder, not inspect the slice it already built.
func TestTheEntryCountIsBoundedWhileItIsRead(t *testing.T) {
	// The blob is built first: the writer enforces the same bound, so an
	// index of three entries cannot even be produced once the cap is two.
	var blob bytes.Buffer
	if err := WriteIndex(&blob, &Index{SchemaVersion: SchemaVersion, Entries: shapeEntries()}, nil); err != nil {
		t.Fatal(err)
	}

	restore := maxIndexEntries
	maxIndexEntries = 2
	defer func() { maxIndexEntries = restore }()

	idx, err := ReadIndex(bytes.NewReader(blob.Bytes()), crypt.NewClearOpener())
	if !errors.Is(err, ErrBadSchema) {
		t.Fatalf("ReadIndex = %v (idx %v), want ErrBadSchema", err, idx)
	}
	if !strings.Contains(err.Error(), "more than 2 entries") {
		t.Fatalf("the refusal must name the bound, got %q", err)
	}

	maxIndexEntries = 3
	if _, err := ReadIndex(bytes.NewReader(blob.Bytes()), crypt.NewClearOpener()); err != nil {
		t.Fatalf("exactly the bound must still read: %v", err)
	}
}
