package recovery

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/archive"
	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
)

const fixturePassphrase = "correct horse battery staple"

type fixture struct {
	root         string
	sourcePath   string
	tarBytes     []byte
	entries      []index.FileEntry
	plainDigests []string
	chunkPath    string
	chunkCount   int
}

// makeFixture writes a schema 1 backup: an encrypted one keeps its
// confidential metadata in the clear, exactly like images produced before the
// private blob existed.
func makeFixture(t *testing.T, encrypted bool, chunkBytes int) fixture {
	t.Helper()
	return buildFixture(t, encrypted, chunkBytes, false)
}

// makePrivateFixture writes the current layout: an encrypted backup whose
// confidential metadata lives in the sealed private blob (schema 2).
func makePrivateFixture(t *testing.T, chunkBytes int) fixture {
	t.Helper()
	return buildFixture(t, true, chunkBytes, true)
}

func buildFixture(t *testing.T, encrypted bool, chunkBytes int, privateMeta bool) fixture {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte(strings.Repeat("beta", 700)), 0o600); err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	aw := archive.NewWriter(&plain, archive.Options{Strict: true, NumericOwner: true})
	if err := aw.AddRoot(ctx, src); err != nil {
		t.Fatal(err)
	}
	if _, err := aw.Close(); err != nil {
		t.Fatal(err)
	}

	codec, err := compress.Get("store")
	if err != nil {
		t.Fatal(err)
	}
	var km *crypt.KeyMaterial
	var sealer crypt.Sealer
	if encrypted {
		km, err = crypt.NewKeyMaterial()
		if err != nil {
			t.Fatal(err)
		}
		defer km.Wipe()
		sealer, err = crypt.NewSealer(km, crypt.NonceRandom)
		if err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := "backup/data/000000.blob"
	var stored bytes.Buffer
	var rows []index.Chunk
	for start, i := 0, 0; start < plain.Len(); start, i = start+chunkBytes, i+1 {
		end := start + chunkBytes
		if end > plain.Len() {
			end = plain.Len()
		}
		chunk := plain.Bytes()[start:end]
		ph := sha256.Sum256(chunk)
		blob := append([]byte(nil), chunk...)
		if encrypted {
			blob, err = sealer.Seal(nil, uint32(i), codec, chunk, ph)
			if err != nil {
				t.Fatal(err)
			}
		}
		sh := sha256.Sum256(blob)
		rows = append(rows, index.Chunk{
			I: i, P: path, Ps: "sha256:" + hex.EncodeToString(ph[:]), Ss: "sha256:" + hex.EncodeToString(sh[:]),
			Pb: int64(len(chunk)), Sb: int64(len(blob)),
		})
		stored.Write(blob)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "000000.blob"), stored.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	// Capture the plaintext digests before SplitPrivate strips them from rows.
	plainDigests := make([]string, len(rows))
	for i, row := range rows {
		plainDigests[i] = row.Ps
	}

	entries := make([]index.FileEntry, 0, len(aw.Entries()))
	for _, e := range aw.Entries() {
		entries = append(entries, index.FileEntry{
			Path: e.Path, Type: fixtureType(e.Type), Size: e.Size,
			Mode: index.FormatMode(uint32(e.Mode.Perm())), UID: e.UID, GID: e.GID,
			MTime: e.ModTime, LinkTarget: e.LinkTarget, TarOffset: e.TarOffset, SHA256: e.SHA256,
		})
	}
	m := &index.Manifest{
		SchemaVersion: 1, Tool: index.ToolInfo{Name: "backimage", Version: "test"},
		CreatedAt: time.Unix(1, 0).UTC(), Sources: []string{src},
		Totals:   index.Totals{Files: 2, Dirs: 2, BytesRaw: int64(len("alpha\n") + 2800), BytesStored: int64(stored.Len())},
		Archive:  index.ArchiveInfo{Format: "tar", Compression: "store"},
		Chunking: index.ChunkingInfo{Strategy: "length", TargetChunkBytes: int64(chunkBytes), Count: len(rows)},
		Layers:   []index.LayerInfo{{Index: 0, Digest: "sha256:fixture", ChunkFrom: 0, ChunkTo: len(rows) - 1, StoredBytes: int64(stored.Len())}},
		Index:    index.Ref{Path: "index.json.zst", Encrypted: encrypted},
	}
	if encrypted {
		m.Encryption = index.EncryptionInfo{Enabled: true, AEAD: "aes256-gcm", KDF: "scrypt-age", NonceMode: "random"}
	}
	table := &index.ChunkTable{SchemaVersion: 1, Chunks: rows}
	if privateMeta {
		private := index.SplitPrivate(m, table)
		if private == nil {
			t.Fatal("SplitPrivate returned nothing for an encrypted fixture")
		}
		writeFile(t, filepath.Join(root, index.PrivatePath), func(w io.Writer) error {
			return index.WritePrivate(w, private, sealer)
		})
	}
	writeFile(t, filepath.Join(root, "manifest.json"), func(w io.Writer) error { return index.WriteManifest(w, m) })
	writeFile(t, filepath.Join(root, "chunks.json"), func(w io.Writer) error { return index.WriteChunkTable(w, table) })
	idx := &index.Index{SchemaVersion: m.SchemaVersion, Entries: entries}
	writeFile(t, filepath.Join(root, "index.json.zst"), func(w io.Writer) error { return index.WriteIndex(w, idx, sealer) })
	if encrypted {
		writeFile(t, filepath.Join(root, "keys.pass.age"), func(w io.Writer) error {
			return crypt.WrapKeys(w, km, crypt.Recipients{Passphrase: []byte(fixturePassphrase)})
		})
	}
	return fixture{
		root: root, sourcePath: src, tarBytes: append([]byte(nil), plain.Bytes()...),
		entries: entries, plainDigests: plainDigests,
		chunkPath: filepath.Join(root, "data", "000000.blob"), chunkCount: len(rows),
	}
}

func writeFile(t *testing.T, path string, write func(io.Writer) error) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := write(f); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func fixtureType(t archive.EntryType) string {
	switch t {
	case archive.TypeDir:
		return index.TypeDir
	case archive.TypeSymlink:
		return index.TypeSymlink
	case archive.TypeHardlink:
		return index.TypeHardlink
	case archive.TypeCharDevice:
		return index.TypeChar
	case archive.TypeBlockDevice:
		return index.TypeBlock
	case archive.TypeFifo:
		return index.TypeFifo
	default:
		return index.TypeRegular
	}
}

func TestStreamTarAndIndex(t *testing.T) {
	f := makeFixture(t, false, 4096)
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var got bytes.Buffer
	if err := b.StreamTar(context.Background(), &got, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), f.tarBytes) {
		t.Fatal("reconstructed tar differs")
	}
	idx, err := b.Index(context.Background())
	if err != nil || len(idx.Entries) != len(f.entries) {
		t.Fatalf("Index() = %d entries, %v", len(idx.Entries), err)
	}
	res, err := b.Verify(context.Background(), true, false)
	if err != nil || !res.OK || res.Entries != len(f.entries) {
		t.Fatalf("Verify() = %+v, %v", res, err)
	}
}

func TestEncryptedUnlockAndPartialVerify(t *testing.T) {
	f := makeFixture(t, true, 1536)
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Index(context.Background()); !errors.Is(err, crypt.ErrWrongPassphrase) {
		t.Fatalf("Index without key = %v", err)
	}
	partial, err := b.Verify(context.Background(), false, false)
	if err != nil || !partial.OK || partial.Full {
		t.Fatalf("partial verify = %+v, %v", partial, err)
	}
	if err := b.Unlock(context.Background(), crypt.Identity{Passphrase: []byte("wrong")}); !errors.Is(err, crypt.ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase = %v", err)
	}
	if err := b.Unlock(context.Background(), crypt.Identity{Passphrase: []byte(fixturePassphrase)}); err != nil {
		t.Fatal(err)
	}
	if !b.IsUnlocked() {
		t.Fatal("backup should be unlocked")
	}
	full, err := b.Verify(context.Background(), true, false)
	if err != nil || !full.OK || full.Entries == 0 {
		t.Fatalf("full verify = %+v, %v", full, err)
	}
}

// TestPrivateMetadataHidesContentUntilUnlock is the regression test for the
// confidentiality of the metadata: with the image but not the passphrase,
// nothing describing the plaintext is readable, and everything comes back once
// the backup is unlocked.
func TestPrivateMetadataHidesContentUntilUnlock(t *testing.T) {
	f := makePrivateFixture(t, 1536)
	ctx := context.Background()

	for _, name := range []string{"manifest.json", "chunks.json"} {
		data, err := os.ReadFile(filepath.Join(f.root, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(f.sourcePath)) {
			t.Fatalf("%s leaks the source path %q", name, f.sourcePath)
		}
		for _, digest := range f.plainDigests {
			if bytes.Contains(data, []byte(digest)) {
				t.Fatalf("%s leaks the plaintext digest %s", name, digest)
			}
		}
		for _, entry := range f.entries {
			// Check the archived path as the index stores it. A bare root
			// component is a short temp-dir name ("001") which can appear inside
			// a random hex digest by chance, so it proves nothing either way.
			if !strings.Contains(entry.Path, "/") {
				continue
			}
			if bytes.Contains(data, []byte(entry.Path)) {
				t.Fatalf("%s leaks the archived path %q", name, entry.Path)
			}
		}
	}

	b, err := OpenLocal(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Manifest.SchemaVersion != index.SchemaVersionPrivate || b.Manifest.Private == nil {
		t.Fatalf("manifest = schema %d, private %+v", b.Manifest.SchemaVersion, b.Manifest.Private)
	}
	if b.Manifest.Sources != nil || b.Manifest.Totals != (index.Totals{}) || b.Manifest.Host != (index.HostInfo{}) {
		t.Fatalf("locked manifest describes the content: %+v", b.Manifest)
	}
	for i, c := range b.Chunks.Chunks {
		if c.Ps != "" || c.Pb != 0 {
			t.Fatalf("locked chunk %d describes the plaintext: %+v", i, c)
		}
	}
	// What stays public must keep the no-passphrase integrity check working.
	partial, err := b.Verify(ctx, false, false)
	if err != nil || !partial.OK || partial.Full {
		t.Fatalf("partial verify = %+v, %v", partial, err)
	}

	if err := b.Unlock(ctx, crypt.Identity{Passphrase: []byte(fixturePassphrase)}); err != nil {
		t.Fatal(err)
	}
	if len(b.Manifest.Sources) != 1 || b.Manifest.Sources[0] != f.sourcePath {
		t.Fatalf("sources after unlock = %v", b.Manifest.Sources)
	}
	if b.Manifest.Totals.Files != 2 {
		t.Fatalf("totals after unlock = %+v", b.Manifest.Totals)
	}
	for i, c := range b.Chunks.Chunks {
		if c.Ps != f.plainDigests[i] || c.Pb == 0 {
			t.Fatalf("chunk %d after unlock = %+v", i, c)
		}
	}
	var got bytes.Buffer
	if err := b.StreamTar(ctx, &got, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), f.tarBytes) {
		t.Fatal("reconstructed tar differs")
	}
	full, err := b.Verify(ctx, true, false)
	if err != nil || !full.OK || full.Entries != len(f.entries) {
		t.Fatalf("full verify = %+v, %v", full, err)
	}
	// Selective restore depends on the plaintext prefix sums, which are only
	// known once the private metadata has been merged.
	idx, err := b.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var selected bytes.Buffer
	if err := b.StreamSelectedTar(ctx, idx, idx.Entries[:1], &selected, true); err != nil {
		t.Fatalf("selected tar: %v", err)
	}
	if selected.Len() == 0 {
		t.Fatal("selected tar is empty")
	}
}

func TestPrivateMetadataNeedsTheKey(t *testing.T) {
	f := makePrivateFixture(t, 1536)
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Unlock(context.Background(), crypt.Identity{Passphrase: []byte("wrong")}); !errors.Is(err, crypt.ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase = %v", err)
	}
	if b.Manifest.Sources != nil {
		t.Fatal("a failed unlock revealed the sources")
	}
	if _, err := b.Index(context.Background()); !errors.Is(err, crypt.ErrWrongPassphrase) {
		t.Fatalf("Index without key = %v", err)
	}
}

type countingSource struct {
	Source
	dataOpens int
}

func (s *countingSource) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	if strings.Contains(name, "/data/") {
		s.dataOpens++
	}
	return s.Source.Open(ctx, name)
}

func TestStreamSelectedTarReadsFewChunks(t *testing.T) {
	f := makeFixture(t, false, 8192)
	source := &countingSource{Source: &LocalSource{Root: f.root}}
	b, err := Open(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	idx, err := b.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := index.EntriesMatching(idx, []string{"**/a.txt"}, nil)
	if err != nil || len(selected) != 1 {
		t.Fatalf("selected = %v, %v", selected, err)
	}
	var out bytes.Buffer
	if err := b.StreamSelectedTar(context.Background(), idx, selected, &out, true); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&out)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if len(names) != 2 || !strings.HasSuffix(names[1], "/a.txt") {
		t.Fatalf("selected tar entries = %v", names)
	}
	if source.dataOpens >= 3 {
		t.Fatalf("selected restore opened %d chunks, want < 3", source.dataOpens)
	}
}

func TestVerifyCorruptionAndUnsafePath(t *testing.T) {
	f := makeFixture(t, false, 2048)
	data, err := os.ReadFile(f.chunkPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(f.chunkPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	res, err := b.Verify(context.Background(), false, true)
	if !errors.Is(err, crypt.ErrIntegrity) || res.OK || len(res.Errors) == 0 {
		t.Fatalf("corrupt verify = %+v, %v", res, err)
	}
	if _, err := (&LocalSource{Root: f.root}).Open(context.Background(), "../../etc/passwd"); err == nil {
		t.Fatal("unsafe path accepted")
	}
}
