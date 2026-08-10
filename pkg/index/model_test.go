package index

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/crypt"
)

func pad3(i int) string { return fmt.Sprintf("%06d", i) }

func pad4(n int) string { return fmt.Sprintf("%06d", n) }

func dig(n int) string {
	b := strings.Repeat("0123456789abcdef", 4) // 64 chars
	n %= 64
	return b[n:] + b[:n]
}

func sampleManifest() *Manifest {
	return &Manifest{
		SchemaVersion: SchemaVersion,
		Tool:          ToolInfo{Name: "backimage", Version: "0.1.0"},
		CreatedAt:     time.Date(2026, 8, 8, 18, 34, 12, 0, time.UTC),
		Sources:       []string{"/home/fabio/myfiles"},
		Host:          HostInfo{Hostname: "ws01", OS: "linux", Arch: "amd64"},
		Totals: Totals{
			Files: 42, Dirs: 7, Symlinks: 2, Hardlinks: 1, Devices: 0,
			BytesRaw: 1048576, BytesStored: 524288,
		},
		Archive: ArchiveInfo{Format: "tar-pax", Compression: "zstd", CompressionLevel: 2},
		Encryption: EncryptionInfo{
			Enabled: true, KDF: "scrypt", AEAD: "aes-256-gcm",
			NonceMode: "random", Recipients: []string{"scrypt"},
		},
		Chunking: ChunkingInfo{Strategy: "fixed", TargetChunkBytes: 4194304, Count: 9},
		Layers: []LayerInfo{
			{Index: 0, Digest: "sha256:" + strings.Repeat("ab", 32), ChunkFrom: 0, ChunkTo: 3, StoredBytes: 4096},
			{Index: 1, Digest: "sha256:" + strings.Repeat("cd", 32), ChunkFrom: 4, ChunkTo: 8, StoredBytes: 8192},
		},
		Index: Ref{Path: "backup/index.json.zst.age", StoredSha256: "sha256:" + strings.Repeat("ef", 32), Encrypted: true},
	}
}

func sampleChunkTable() *ChunkTable {
	ct := &ChunkTable{SchemaVersion: SchemaVersion}
	for i := 0; i < 9; i++ {
		ct.Chunks = append(ct.Chunks, Chunk{
			I:  i,
			P:  "backup/data/" + pad3(i) + ".blob",
			Ps: "sha256:" + dig(i),
			Ss: "sha256:" + dig(i+100),
			Pb: int64(1000 + i*100),
			Sb: int64(300 + i*10),
		})
	}
	return ct
}

func sampleIndex() *Index {
	return &Index{
		SchemaVersion: SchemaVersion,
		Entries: []FileEntry{
			{Path: "myfiles/a.txt", Type: TypeRegular, Size: 123, Mode: "0644",
				UID: 1000, GID: 1000, UName: "fabio", GName: "fabio",
				MTime:      time.Date(2026, 8, 1, 10, 0, 0, 123456789, time.UTC),
				LinkTarget: "", TarOffset: 1536, SHA256: dig(1)},
			{Path: "myfiles/sub", Type: TypeDir, Size: 0, Mode: "0755",
				TarOffset: 2048, SHA256: ""},
			{Path: "myfiles/lnk", Type: TypeSymlink, Size: 0, Mode: "0777",
				LinkTarget: "a.txt", TarOffset: 2560, SHA256: ""},
		},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteManifest(&buf, sampleManifest()); err != nil {
		t.Fatal(err)
	}
	back, err := ReadManifest(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if back.Tool.Name != "backimage" || !back.CreatedAt.Equal(sampleManifest().CreatedAt) ||
		back.Index.Encrypted != true || len(back.Layers) != 2 {
		t.Fatalf("manifest round trip lost data: %+v", back)
	}
}

func TestChunkTableRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteChunkTable(&buf, sampleChunkTable()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.TrimRight(buf.String(), "\n"), "\n") {
		t.Fatal("chunk table must be a single compact JSON line")
	}
	back, err := ReadChunkTable(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Chunks) != 9 || back.Chunks[4].Pb != 1400 {
		t.Fatalf("chunk table mismatch: %+v", back.Chunks[4])
	}
}

func TestWriteIndexClearRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteIndex(&buf, sampleIndex(), nil); err != nil {
		t.Fatal(err)
	}
	back, err := ReadIndex(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Entries) != 3 || back.Entries[0].SHA256 != dig(1) ||
		back.Entries[2].Type != TypeSymlink || back.Entries[2].LinkTarget != "a.txt" {
		t.Fatalf("index mismatch: %+v", back.Entries)
	}
}

func TestWriteIndexDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := WriteIndex(&a, sampleIndex(), nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteIndex(&b, sampleIndex(), nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("zstd index output not deterministic")
	}
	if a.Len() > 400 {
		t.Fatalf("index too big (%d bytes): compression may be broken", a.Len())
	}
}

func TestManifestFutureSchema(t *testing.T) {
	m := sampleManifest()
	m.SchemaVersion = 99
	var buf bytes.Buffer
	WriteManifest(&buf, m)
	_, err := ReadManifest(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("schema 99 must be rejected")
	}
	want := "backup created by a more recent backimage: update backimage (schema 99, supported 1)"
	if err.Error() != want {
		t.Fatalf("error text:\n got %q\nwant %q", err.Error(), want)
	}
}

func TestReadManifestInvalid(t *testing.T) {
	bogus := []func() *Manifest{
		func() *Manifest { return &Manifest{SchemaVersion: SchemaVersion} },
		func() *Manifest { m := sampleManifest(); m.CreatedAt = time.Time{}; return m },
		func() *Manifest { m := sampleManifest(); m.Archive.Format = ""; return m },
		func() *Manifest { m := sampleManifest(); m.Index.Path = ""; return m },
		func() *Manifest {
			m := sampleManifest()
			m.Layers = []LayerInfo{{Digest: "", ChunkFrom: 1, ChunkTo: 0}}
			return m
		},
	}
	for i, mk := range bogus {
		var buf bytes.Buffer
		WriteManifest(&buf, mk())
		if _, err := ReadManifest(bytes.NewReader(buf.Bytes())); err == nil {
			t.Fatalf("case %d must be rejected", i)
		}
	}
}

func TestChunkTableInvalid(t *testing.T) {
	bad := sampleChunkTable()
	bad.Chunks[3].I = 7
	var buf bytes.Buffer
	WriteChunkTable(&buf, bad)
	if _, err := ReadChunkTable(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("non-contiguous chunk indices must be rejected")
	}
	bad.Chunks[3].I = 3
	bad.Chunks[3].Ps = "garbage"
	buf.Reset()
	WriteChunkTable(&buf, bad)
	if _, err := ReadChunkTable(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("bad digest must be rejected")
	}
}

func TestGoldenFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		gen  func(w *os.File) error
	}{
		{"manifest", "testdata/manifest.golden.json", func(w *os.File) error { return WriteManifest(w, sampleManifest()) }},
		{"chunkTable", "testdata/chunks.golden.json", func(w *os.File) error { return WriteChunkTable(w, sampleChunkTable()) }},
		{"index", "testdata/index.golden.zst", func(w *os.File) error { return WriteIndex(w, sampleIndex(), nil) }},
	} {
		got := &bytes.Buffer{}
		f, err := os.CreateTemp(t.TempDir(), "g")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := tc.gen(f); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := got.ReadFrom(f); err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("missing golden %s: regenerate it", tc.path)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("golden drift in %s:\n got %q\nwant %q", tc.path, got.Bytes(), want)
		}
	}
}

func TestIndexEndToEndEncrypted(t *testing.T) {
	km, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()
	sealer, err := crypt.NewSealer(km, crypt.NonceRandom)
	if err != nil {
		t.Fatal(err)
	}
	opener, err := crypt.NewOpener(km)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteIndex(&buf, sampleIndex(), sealer); err != nil {
		t.Fatal(err)
	}
	back, err := ReadIndex(bytes.NewReader(buf.Bytes()), opener)
	if err != nil || len(back.Entries) != 3 {
		t.Fatalf("encrypted index round trip: %v", err)
	}
	bad := append([]byte(nil), buf.Bytes()...)
	bad[len(bad)/2] ^= 1
	if _, err := ReadIndex(bytes.NewReader(bad), opener); err == nil {
		t.Fatal("tampered index must fail")
	}
}

func TestWriteIndexInvalidEntries(t *testing.T) {
	hex64 := strings.Repeat("cd", 32)
	cases := []*Index{
		{SchemaVersion: 1, Entries: []FileEntry{{Path: "", Type: TypeRegular, Mode: "0644"}}},
		{SchemaVersion: 1, Entries: []FileEntry{{Path: "a", Type: "bogus", Mode: "0644"}}},
		{SchemaVersion: 1, Entries: []FileEntry{{Path: "a", Type: TypeRegular, Mode: "0644", Size: -1}}},
		{SchemaVersion: 1, Entries: []FileEntry{{Path: "a", Type: TypeRegular, Mode: "bad", SHA256: hex64}}},
		{SchemaVersion: 1, Entries: []FileEntry{{Path: "a", Type: TypeRegular, Mode: "0644", SHA256: "zz"}}},
		{SchemaVersion: 1, Entries: []FileEntry{{Path: "a", Type: TypeSymlink, Mode: "0777", LinkTarget: ""}}},
		{SchemaVersion: 99, Entries: nil},
	}
	for i, idx := range cases {
		var buf bytes.Buffer
		if err := WriteIndex(&buf, idx, nil); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestReadIndexCorrupt(t *testing.T) {
	if _, err := ReadIndex(bytes.NewReader([]byte("junk")), nil); err == nil {
		t.Fatal("junk must fail")
	}
	if _, err := ReadIndex(bytes.NewReader([]byte(`{`)), nil); err == nil {
		t.Fatal("unfinished object must fail")
	}
	if _, err := ReadIndex(bytes.NewReader([]byte(`{"schemaVersion":99,"entries":[]}`+"\n")), nil); err == nil {
		t.Fatal("schema 99 must fail")
	}
	// wrong key on an encrypted index
	kmRaw, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	km, _ := crypt.NewSealer(kmRaw, crypt.NonceRandom)
	var buf bytes.Buffer
	if err := WriteIndex(&buf, &Index{SchemaVersion: 1}, km); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndex(bytes.NewReader(buf.Bytes()), mustOpener(t)); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
}

func mustOpener(t *testing.T) crypt.Opener {
	t.Helper()
	km := &crypt.KeyMaterial{SchemaVersion: 1, DEK: make([]byte, 32), NonceKey: make([]byte, 32)}
	o, err := crypt.NewOpener(km)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestFormatMode(t *testing.T) {
	for in, want := range map[uint32]string{
		0o644: "0644", 0o755: "0755", 0o4777: "04777", 0o5: "0005", 0: "0000",
	} {
		if got := FormatMode(in); got != want {
			t.Errorf("FormatMode(%#o) = %s, want %s", in, got, want)
		}
	}
	if _, err := ParseMode("644"); err == nil {
		t.Error("must reject mode without leading zero")
	}
	if _, err := ParseMode("0123456"); err == nil {
		t.Error("must reject 7-digit mode")
	}
	if _, err := ParseMode("08"); err == nil {
		t.Error("must reject non-octal digit")
	}
	if _, err := ParseMode(""); err == nil {
		t.Error("must reject empty mode")
	}
	if v, err := ParseMode("04755"); err != nil || v != 0o4755 {
		t.Errorf("ParseMode(04755) = %o, %v", v, err)
	}
}

func TestReadIndexStreamingErrors(t *testing.T) {
	cases := []struct {
		name string
		blob string
	}{
		{"notobject", `[1,2]`},
		{"entriesNotArray", `{"schemaVersion":1,"entries":7}`},
		{"badEntry", `{"schemaVersion":1,"entries":[{"path":1}]}`},
		{"truncated", `{"schemaVersion":1,`},
		{"badkeyskip", `{"schemaVersion":1,"weird":`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ReadIndex(bytes.NewReader([]byte(c.blob)), nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, fmt.Errorf("boom") }

func TestWriterFailurePaths(t *testing.T) {
	var w errWriter
	if err := WriteChunkTable(w, sampleChunkTable()); err == nil {
		t.Fatal("expected write error")
	}
	if err := WriteManifest(w, sampleManifest()); err == nil {
		t.Fatal("expected write error")
	}
	if err := WriteIndex(w, &Index{SchemaVersion: 1}, nil); err == nil {
		t.Fatal("expected write error")
	}
}
