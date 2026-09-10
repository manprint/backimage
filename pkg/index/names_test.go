package index

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/crypt"
)

// The index carries a filename byte for byte, as long as the name is valid
// UTF-8: backslashes, quotes, newlines, spaces and both Unicode normal forms
// survive the JSON round trip unchanged.
func TestIndexCarriesHostileNamesUnchanged(t *testing.T) {
	names := []string{
		"back\\slash.txt",
		"quote'and\"double.txt",
		"line\nbreak.txt",
		"tab\there.txt",
		"é-decomposed.txt",
		"é-precomposed.txt",
		"emoji-\U0001f643.txt",
		" leading and trailing .txt",
	}
	idx := &Index{SchemaVersion: SchemaVersion}
	for i, name := range names {
		idx.Entries = append(idx.Entries, FileEntry{
			Path: name, Type: TypeRegular, Size: int64(i), Mode: "0644",
			MTime: time.Unix(0, 0).UTC(), TarOffset: int64(i) * 512, SHA256: strings.Repeat("a", 64),
		})
	}
	var buf bytes.Buffer
	if err := WriteIndex(&buf, idx, nil); err != nil {
		t.Fatal(err)
	}
	back, err := ReadIndex(bytes.NewReader(buf.Bytes()), crypt.NewClearOpener())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Entries) != len(names) {
		t.Fatalf("read back %d entries, wrote %d", len(back.Entries), len(names))
	}
	for i, name := range names {
		if back.Entries[i].Path != name {
			t.Errorf("entry %d: path %q, want %q", i, back.Entries[i].Path, name)
		}
	}
}

// A name that is not valid UTF-8 is the one case the index cannot carry: JSON
// has no encoding for those bytes, and Go substitutes U+FFFD for each of them.
//
// The tar itself keeps the real bytes, so the file is archived and restored
// intact; what is lost is the index's record of its name, which is what `ls`,
// `find` and a selective restore match against. Fixing it means encoding paths
// differently, which is a format change and belongs to phase A6. Until then
// this test states the limit out loud so it cannot be rediscovered as a
// surprise — it will fail the day the encoding changes, and the documentation
// has to change with it. See docs/FIDELITY.md and plan/astra/bugs.md B-A002.
func TestNonUTF8NamesDoNotSurviveTheIndex(t *testing.T) {
	const raw = "non-utf8-\xff\xfe.txt"
	idx := &Index{SchemaVersion: SchemaVersion, Entries: []FileEntry{{
		Path: raw, Type: TypeRegular, Size: 1, Mode: "0644",
		MTime: time.Unix(0, 0).UTC(), TarOffset: 0, SHA256: strings.Repeat("a", 64),
	}}}
	var buf bytes.Buffer
	if err := WriteIndex(&buf, idx, nil); err != nil {
		t.Fatal(err)
	}
	back, err := ReadIndex(bytes.NewReader(buf.Bytes()), crypt.NewClearOpener())
	if err != nil {
		t.Fatal(err)
	}
	got := back.Entries[0].Path
	if got == raw {
		t.Fatal("non-UTF-8 names now survive the index: update docs/FIDELITY.md and remove this test")
	}
	if want := "non-utf8-��.txt"; got != want {
		t.Fatalf("path = %q, want the replacement-character form %q", got, want)
	}
}
