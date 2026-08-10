package server

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fpierri/backimage/pkg/archive"
	"github.com/fpierri/backimage/pkg/index"
)

// TestScanArchiveMatchesLocalWriter locks the invariant the streaming protocol
// depends on: the index the server derives from the raw stream is the same one
// the client used to build locally.
func TestScanArchiveClassifiesEveryEntryType(t *testing.T) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	headers := []*tar.Header{
		{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "file", Typeflag: tar.TypeReg, Mode: 0o4755, Size: 3},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "file"},
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "file"},
		{Name: "chr", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
		{Name: "blk", Typeflag: tar.TypeBlock, Devmajor: 8, Devminor: 0},
		{Name: "pipe", Typeflag: tar.TypeFifo},
	}
	for _, header := range headers {
		header.Format = tar.FormatPAX
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte("abc")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	entries, stats, err := scanArchive(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{index.TypeDir, index.TypeRegular, index.TypeSymlink, index.TypeHardlink, index.TypeChar, index.TypeBlock, index.TypeFifo}
	if len(entries) != len(want) {
		t.Fatalf("entries = %d, want %d", len(entries), len(want))
	}
	for i, kind := range want {
		if entries[i].Type != kind {
			t.Fatalf("entry %d type = %q, want %q", i, entries[i].Type, kind)
		}
	}
	if entries[1].Mode != "04755" {
		t.Fatalf("setuid mode = %q", entries[1].Mode)
	}
	if stats.Files != 1 || stats.Dirs != 1 || stats.Symlinks != 1 || stats.Hardlinks != 1 || stats.Devices != 3 || stats.BytesRaw != 3 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestScanArchiveRejectsTruncatedStream(t *testing.T) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Size: 1024, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scanArchive(bytes.NewReader(buf.Bytes()[:600])); err == nil {
		t.Fatal("a truncated stream was accepted")
	}
}

func TestScanArchiveMatchesLocalWriter(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("scan-parity-"), 4096)
	for name, data := range map[string][]byte{
		"a.txt":       []byte("first"),
		"sub/big.bin": payload,
		"sub/empty":   nil,
	} {
		if err := os.WriteFile(filepath.Join(tree, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	writer := archive.NewWriter(&buf, archive.Options{Strict: true, PreserveXattrs: true, PreserveACLs: true})
	if err := writer.AddRoot(context.Background(), tree); err != nil {
		t.Fatal(err)
	}
	stats, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := writer.Entries()

	got, scanned, err := scanArchive(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Path != want[i].Path {
			t.Fatalf("entry %d path = %q, want %q", i, got[i].Path, want[i].Path)
		}
		if got[i].TarOffset != want[i].TarOffset {
			t.Fatalf("entry %q tar offset = %d, want %d", want[i].Path, got[i].TarOffset, want[i].TarOffset)
		}
		if got[i].SHA256 != want[i].SHA256 {
			t.Fatalf("entry %q sha256 = %q, want %q", want[i].Path, got[i].SHA256, want[i].SHA256)
		}
		if got[i].Size != want[i].Size {
			t.Fatalf("entry %q size = %d, want %d", want[i].Path, got[i].Size, want[i].Size)
		}
	}
	if scanned.Files != stats.Files || scanned.Dirs != stats.Dirs || scanned.Symlinks != stats.Symlinks || scanned.BytesRaw != stats.BytesRaw {
		t.Fatalf("stats = %+v, want files=%d dirs=%d symlinks=%d bytes=%d",
			scanned, stats.Files, stats.Dirs, stats.Symlinks, stats.BytesRaw)
	}
}
