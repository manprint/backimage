package ociimg

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/fpierri/backimage/pkg/compress"
)

func fileOpen(payload []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
}

func sampleFiles() []LayerFile {
	return []LayerFile{
		{Path: "/backup/manifest.json", Mode: 0o644, Size: 12, Open: fileOpen([]byte("hello worldx"))},
		{Path: "/backup/data/000000.blob", Mode: 0o444, Size: 5, Open: fileOpen([]byte("abcde"))},
		{Path: "/backup/data/000001.blob", Mode: 0o444, Size: 5, Open: fileOpen([]byte("fghij"))},
	}
}

func TestBuildLayerTarDeterministic(t *testing.T) {
	a, b := &bytes.Buffer{}, &bytes.Buffer{}
	if err := BuildLayerTar(a, sampleFiles()); err != nil {
		t.Fatal(err)
	}
	if err := BuildLayerTar(b, sampleFiles()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two identical invocations must produce identical bytes")
	}
	da := sha256.Sum256(a.Bytes())
	db := sha256.Sum256(b.Bytes())
	if da != db {
		t.Fatal("SHA-256 digest must be identical")
	}
}

func TestBuildLayerTarReadable(t *testing.T) {
	var buf bytes.Buffer
	if err := BuildLayerTar(&buf, sampleFiles()); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	var names []string
	fileData := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			fileData[hdr.Name] = string(b)
		}
	}
	// dirs first, then files in input order
	want := []string{"/backup", "/backup/data", "/backup/manifest.json", "/backup/data/000000.blob", "/backup/data/000001.blob"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entry order:\n got %v\nwant %v", names, want)
	}
	if fileData["/backup/manifest.json"] != "hello worldx" {
		t.Fatalf("file content: %q", fileData["/backup/manifest.json"])
	}
	if fileData["/backup/data/000001.blob"] != "fghij" {
		t.Fatal("second blob content wrong")
	}
}

func TestBuildLayerTarOwnership(t *testing.T) {
	var buf bytes.Buffer
	BuildLayerTar(&buf, sampleFiles())
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 {
			t.Fatalf("%s: uid/gid not locked to 0", hdr.Name)
		}
		if hdr.ModTime.Unix() != 0 {
			t.Fatalf("%s: mtime must be epoch, got %v", hdr.Name, hdr.ModTime)
		}
		if hdr.Uname != "" || hdr.Gname != "" {
			t.Fatalf("%s: uname/gname must be empty", hdr.Name)
		}
		if hdr.Format == tar.FormatPAX {
			t.Fatalf("%s: unexpected PAX record", hdr.Name)
		}
	}
}

func TestBuildLayerTarSizeMismatch(t *testing.T) {
	files := []LayerFile{
		{Path: "/x", Mode: 0o644, Size: 3, Open: fileOpen([]byte("toolong"))},
	}
	if err := BuildLayerTar(&bytes.Buffer{}, files); err == nil {
		t.Fatal("size mismatch must error")
	}
}

func TestUSTAR118Files(t *testing.T) {
	// 118 files whose names are 6 digits: all names fit USTAR.
	var files []LayerFile
	for i := 0; i < 118; i++ {
		name := fmt.Sprintf("/backup/data/%06d.blob", i)
		files = append(files, LayerFile{Path: name, Mode: 0o644, Size: 1, Open: fileOpen([]byte("x"))})
	}
	var buf bytes.Buffer
	if err := BuildLayerTar(&buf, files); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Format == tar.FormatPAX {
			t.Fatalf("%s must stay USTAR", hdr.Name)
		}
		n++
	}
	if n != 118+2 { // 118 files + /backup + /backup/data
		t.Fatalf("entries %d, want 120", n)
	}
}

func TestNewLayer(t *testing.T) {
	codec, err := compress.ByID(compress.Zstd)
	if err != nil {
		t.Fatal(err)
	}
	l1, err := NewLayer(sampleFiles(), codec, 2)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := NewLayer(sampleFiles(), codec, 2)
	if err != nil {
		t.Fatal(err)
	}
	h1, _ := l1.Digest()
	h2, _ := l2.Digest()
	if h1 != h2 {
		t.Fatalf("digests must match: %s vs %s", h1, h2)
	}
	// digest matches the actual stored bytes
	rc, _ := l1.Compressed()
	got, _ := io.ReadAll(rc)
	if h1 != sha256Of(got) {
		t.Fatal("Digest() must equal sha256 of Compressed()")
	}
	// decompression round trip through the codec
	urc, err := l1.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(urc)
	tr := tar.NewReader(bytes.NewReader(raw))
	if _, err := tr.Next(); err != nil {
		t.Fatalf("raw tar unreadable: %v", err)
	}
	mt, _ := l1.MediaType()
	if !strings.HasSuffix(string(mt), "+zstd") {
		t.Fatalf("media type %q must carry +zstd", mt)
	}
}

func TestNewLayerStoreNoSuffix(t *testing.T) {
	codec, err := compress.ByID(compress.Store)
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewLayer(sampleFiles(), codec, 0)
	if err != nil {
		t.Fatal(err)
	}
	mt, _ := l.MediaType()
	if string(mt) != "application/vnd.oci.image.layer.v1.tar" {
		t.Fatalf("store layer media type %q", mt)
	}
}

func TestNewLayerPAXLongNames(t *testing.T) {
	long := strings.Repeat("d", 300) + "/file.txt"
	codec := mustGzipCodec(t)
	l, err := NewLayer([]LayerFile{{
		Path: long,
		Mode: 0o644,
		Size: 3,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("abc")), nil },
	}}, codec, 1)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := l.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	var hdr *tar.Header
	for {
		hdr, err = tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			break
		}
	}
	if hdr.Format != tar.FormatPAX {
		t.Fatalf("format = %v, want PAX", hdr.Format)
	}
	content, _ := io.ReadAll(tr)
	if string(content) != "abc" {
		t.Fatalf("content = %q", content)
	}
}

func TestFormatForNonASCII(t *testing.T) {
	if formatFor("caf\xc3\xa9.txt") != tar.FormatPAX {
		t.Fatal("non-ascii name must be PAX")
	}
	if formatFor("ok.txt") != tar.FormatUSTAR {
		t.Fatal("short ascii name must be USTAR")
	}
}

func mustGzipCodec(t *testing.T) compress.Codec {
	t.Helper()
	c, err := compress.ByID(compress.Gzip)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
