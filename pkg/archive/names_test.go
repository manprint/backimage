//go:build unix

package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// hostileNames are the names a Unix filesystem accepts and a naive archiver
// mangles. A backslash is the one that used to be lost: CleanPath turned it
// into a separator, so the single file `back\slash.txt` came out of a restore
// as the directory `back` holding `slash.txt`.
var hostileNames = []string{
	"back\\slash.txt",
	"two\\\\backslashes.txt",
	"trailing\\",
	"quote'and\"double.txt",
	"line\nbreak.txt",
	"tab\there.txt",
	"ünïcödé.txt",
	"e\u0301-decomposed.txt", // NFD: e + combining acute
	"\u00e9-precomposed.txt", // NFC: the same grapheme, a different name
	"emoji-\U0001f643.txt",
	"non-utf8-\xff\xfe.txt",
	" leading-space.txt",
	"trailing-space .txt",
	"-dash-start.txt",
}

// The archive must carry a filename byte for byte: what goes in comes out with
// the same bytes, through the writer, the reader and the extractor.
func TestHostileNamesRoundTripByteForByte(t *testing.T) {
	src := t.TempDir()
	for _, name := range hostileNames {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o644); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	archived := buf.Bytes()

	// 1. The reader sees the same names, under the root directory's own name.
	base := filepath.Base(src)
	r := NewReader(bytes.NewReader(archived))
	var got []string
	for {
		e, body, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			t.Fatal(err)
		}
		if e.Type == TypeRegular {
			got = append(got, e.Path)
		}
	}
	want := make([]string, 0, len(hostileNames))
	for _, name := range hostileNames {
		want = append(want, base+"/"+name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("the archive holds %d regular files, the tree has %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("archived name %q, on disk %q", got[i], want[i])
		}
	}

	// 2. The extractor puts the same bytes back on disk.
	dst := t.TempDir()
	if _, err := NewExtractor(ExtractOptions{Strict: true}).
		Extract(context.Background(), bytes.NewReader(archived), dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dst, base))
	if err != nil {
		t.Fatal(err)
	}
	restored := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			restored = append(restored, e.Name())
		}
	}
	sorted := append([]string(nil), hostileNames...)
	sort.Strings(sorted)
	sort.Strings(restored)
	if len(restored) != len(sorted) {
		t.Fatalf("restored %d files, archived %d:\n%q", len(restored), len(sorted), restored)
	}
	for i := range sorted {
		if restored[i] != sorted[i] {
			t.Errorf("restored %q, want %q", restored[i], sorted[i])
		}
	}
	// A backslash must not have become a directory level.
	if fi, err := os.Stat(filepath.Join(dst, base, "back")); err == nil && fi.IsDir() {
		t.Error(`the name "back\slash.txt" was split into a directory`)
	}
	for _, name := range hostileNames {
		content, err := os.ReadFile(filepath.Join(dst, base, name))
		if err != nil {
			t.Errorf("read back %q: %v", name, err)
			continue
		}
		if string(content) != name {
			t.Errorf("content of %q is %q", name, content)
		}
	}
}
