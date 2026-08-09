package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateEntry(t *testing.T) {
	base := &Entry{Path: "a/b", Type: TypeRegular, Size: 5, Mode: 0o644}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	bad := []*Entry{
		{},
		{Path: "/abs", Type: TypeRegular},
		{Path: "../dotdot", Type: TypeRegular},
		{Path: "a/../b", Type: TypeRegular},
		{Path: "a", Type: TypeDir, Size: 9},
		{Path: "l", Type: TypeSymlink, LinkTarget: ""},
		{Path: "h", Type: TypeHardlink, LinkTarget: ""},
		{Path: "x", Type: TypeRegular, Xattrs: map[string][]byte{"": []byte{1}}},
	}
	for i, e := range bad {
		if err := e.Validate(); err == nil {
			t.Errorf("case %d (%q): expected error, got nil", i, e.Path)
		}
	}
}

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"a\\b\\c": "a/b/c",
		"a//b":    "a/b",
		"a/./b":   "a/b",
		"":        ".",
		"dir/":    "dir",
	}
	for in, want := range cases {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePAXTime(t *testing.T) {
	cases := []string{
		"1700000000.123456789",
		"0.500",
		"1700000000",
	}
	for _, c := range cases {
		tt, err := parsePAXTime(c)
		if err != nil {
			t.Errorf("parsePAXTime(%q): %v", c, err)
			continue
		}
		if tt.IsZero() {
			t.Errorf("parsePAXTime(%q) zero", c)
		}
	}
	if _, err := parsePAXTime("notatime"); err == nil {
		t.Error("garbage time must error")
	}
}

func TestReaderEOFAndUnsupported(t *testing.T) {
	r := NewReader(bytes.NewReader(nil))
	_, _, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("empty archive: want io.EOF, got %v", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "pax", Typeflag: tar.TypeXGlobalHeader})
	tw.Close()
	r2 := NewReader(&buf)
	if _, _, err := r2.Next(); err == nil {
		t.Error("unsupported typeflag must error")
	}
}

func TestReaderAtimeCtimeRoundTrip(t *testing.T) {
	src := t.TempDir()
	f := filepath.Join(src, "f")
	os.WriteFile(f, []byte("x"), 0o644)
	mt := time.Unix(1710000000, 111222333)
	at := time.Unix(1710000001, 444555666)
	os.Chtimes(f, at, mt)

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{PreserveTimes: true})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()
	r := NewReader(&buf)
	found := false
	for {
		e, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(e.Path) == "f" {
			found = true
			if e.ModTime.UnixNano() != mt.UnixNano() {
				t.Errorf("mtime = %v want %v", e.ModTime, mt)
			}
			if e.AccessTime.UnixNano() != at.UnixNano() {
				t.Errorf("atime = %v want %v", e.AccessTime, at)
			}
			if e.ChangeTime.IsZero() {
				t.Errorf("ctime not read")
			}
		}
	}
	if !found {
		t.Fatal("file entry not found")
	}
}

func TestExtractFifoAndSymlinkChain(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0o600})
	tw.WriteHeader(&tar.Header{Name: "linka", Typeflag: tar.TypeSymlink, Linkname: "real", Mode: 0o777})
	tw.WriteHeader(&tar.Header{Name: "linkb", Typeflag: tar.TypeSymlink, Linkname: "linka", Mode: 0o777})
	tw.WriteHeader(&tar.Header{Name: "real", Typeflag: tar.TypeReg, Mode: 0o640, Size: 2})
	tw.Write([]byte("ok"))
	tw.Close()

	x := NewExtractor(ExtractOptions{Strict: true})
	st, err := x.Extract(context.Background(), &buf, dst)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if st.Fifos != 1 || st.Symlinks != 2 || st.Files != 1 {
		t.Errorf("stats %+v", st)
	}
	fi, err := os.Lstat(filepath.Join(dst, "pipe"))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("fifo not created: %v %v", err, fi)
	}
	tgt, err := os.Readlink(filepath.Join(dst, "linkb"))
	if err != nil || tgt != "linka" {
		t.Errorf("symlink chain broken: %v %q", err, tgt)
	}
}

func TestExtractIncludesOnly(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"skip", "keep"} {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
		tw.Write([]byte("x"))
	}
	tw.Close()
	x := NewExtractor(ExtractOptions{Includes: []string{"keep"}, Strict: true})
	st, err := x.Extract(context.Background(), &buf, dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 1 {
		t.Errorf("files = %d, want 1", st.Files)
	}
	if _, err := os.Lstat(filepath.Join(dst, "skip")); err == nil {
		t.Error("skip must not be extracted")
	}
}

func TestReaderNextError(t *testing.T) {
	r := NewReader(strings.NewReader("not a tar"))
	_, _, err := r.Next()
	if err == nil {
		t.Fatal("garbage input must error")
	}
}

func TestWriterCloseOnDiscard(t *testing.T) {
	w := NewWriter(io.Discard, Options{})
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("disk full")
	}
	n := len(p)
	if n > f.n {
		n = f.n
	}
	f.n -= n
	return n, nil
}

func TestWriterCloseTrailerError(t *testing.T) {
	fw := &failingWriter{n: 7 * 1024}
	w := NewWriter(fw, Options{})
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "f"), bytes.Repeat([]byte("x"), 4096), 0o644)
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatalf("entries must fit: %v", err)
	}
	if _, err := w.Close(); err == nil {
		t.Fatal("trailer write failure must surface on Close")
	}
}

func TestReaderNextMidStreamError(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "a", Size: 4096, Mode: 0o644})
	tw.Write(bytes.Repeat([]byte("x"), 4096))
	tw.Close()
	r := NewReader(&brokenReader{r: &buf, cut: 512})
	for {
		_, _, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("must not reach EOF")
			}
			break
		}
	}
}

type brokenReader struct {
	r   io.Reader
	cut int
}

func (b *brokenReader) Read(p []byte) (int, error) {
	if b.cut <= 0 {
		return 0, errors.New("network reset")
	}
	if len(p) > b.cut {
		p = p[:b.cut]
	}
	b.cut -= len(p)
	return b.r.Read(p)
}

func TestReaderTypeflagMapping(t *testing.T) {
	types := []struct {
		flag byte
		typ  EntryType
	}{
		{tar.TypeLink, TypeHardlink},
		{tar.TypeBlock, TypeBlockDevice},
		{'Z', 0},
	}
	for _, tc := range types {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		hdr := &tar.Header{Name: "x", Typeflag: tc.flag, Mode: 0o600}
		if tc.flag == tar.TypeLink {
			hdr.Linkname = "target"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		// give linkname for hardlink
		tw.Close()
		r := NewReader(&buf)
		e, _, err := r.Next()
		if tc.typ == 0 {
			if err == nil {
				t.Fatalf("typeflag %q: unsupported must error", tc.flag)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.Type != tc.typ {
			t.Fatalf("typeflag %q: type = %v, want %v", tc.flag, e.Type, tc.typ)
		}
	}
}
