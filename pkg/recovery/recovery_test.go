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
	root       string
	tarBytes   []byte
	entries    []index.FileEntry
	chunkPath  string
	chunkCount int
}

func makeFixture(t *testing.T, encrypted bool, chunkBytes int) fixture {
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
	writeFile(t, filepath.Join(root, "manifest.json"), func(w io.Writer) error { return index.WriteManifest(w, m) })
	table := &index.ChunkTable{SchemaVersion: 1, Chunks: rows}
	writeFile(t, filepath.Join(root, "chunks.json"), func(w io.Writer) error { return index.WriteChunkTable(w, table) })
	idx := &index.Index{SchemaVersion: 1, Entries: entries}
	writeFile(t, filepath.Join(root, "index.json.zst"), func(w io.Writer) error { return index.WriteIndex(w, idx, sealer) })
	if encrypted {
		writeFile(t, filepath.Join(root, "keys.pass.age"), func(w io.Writer) error {
			return crypt.WrapKeys(w, km, crypt.Recipients{Passphrase: []byte(fixturePassphrase)})
		})
	}
	return fixture{root: root, tarBytes: append([]byte(nil), plain.Bytes()...), entries: entries, chunkPath: filepath.Join(root, "data", "000000.blob"), chunkCount: len(rows)}
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
