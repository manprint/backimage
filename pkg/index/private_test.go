package index

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/crypt"
)

func encryptedManifest() *Manifest {
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		Tool:          ToolInfo{Name: "backimage", Version: "test"},
		CreatedAt:     time.Unix(1, 0).UTC(),
		Sources:       []string{"/srv/secret-project"},
		Host:          HostInfo{Hostname: "vault-01", OS: "linux", Arch: "amd64"},
		Totals:        Totals{Files: 7, Dirs: 2, BytesRaw: 4096, BytesStored: 1024},
		Archive:       ArchiveInfo{Format: "tar", Compression: "zstd", CompressionLevel: 2},
		Encryption: EncryptionInfo{
			Enabled: true, KDF: "scrypt-age", AEAD: "aes256-gcm", NonceMode: "random",
			KeyFingerprint: "0123456789abcdef", Recipients: []string{"age1example"},
		},
		Chunking: ChunkingInfo{Strategy: "length", TargetChunkBytes: 1 << 20, Count: 2},
		Layers:   []LayerInfo{{Index: 0, Digest: "sha256:" + strings.Repeat("ab", 32), ChunkTo: 1, StoredBytes: 1024}},
		Index:    Ref{Path: "index.json.zst", Encrypted: true},
	}
	return m
}

func encryptedChunkTable() *ChunkTable {
	return &ChunkTable{
		SchemaVersion: SchemaVersion,
		Chunks: []Chunk{
			{I: 0, P: "backup/data/aa.blob", Ps: "sha256:" + strings.Repeat("11", 32), Ss: "sha256:" + strings.Repeat("22", 32), Pb: 3000, Sb: 700},
			{I: 1, P: "backup/data/aa.blob", Ps: "sha256:" + strings.Repeat("33", 32), Ss: "sha256:" + strings.Repeat("44", 32), Pb: 1096, Sb: 324},
		},
	}
}

func testSealerOpener(t *testing.T, mode crypt.NonceMode) (crypt.Sealer, crypt.Opener, *crypt.KeyMaterial) {
	t.Helper()
	km, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(km.Wipe)
	sealer, err := crypt.NewSealer(km, mode)
	if err != nil {
		t.Fatal(err)
	}
	opener, err := crypt.NewOpener(km)
	if err != nil {
		t.Fatal(err)
	}
	return sealer, opener, km
}

func TestSplitPrivateStripsAndMergeRestores(t *testing.T) {
	m, table := encryptedManifest(), encryptedChunkTable()
	private := SplitPrivate(m, table)
	if private == nil {
		t.Fatal("SplitPrivate returned nothing for an encrypted backup")
	}
	if m.SchemaVersion != SchemaVersionPrivate || table.SchemaVersion != SchemaVersionPrivate {
		t.Fatalf("schema = %d / %d, want %d", m.SchemaVersion, table.SchemaVersion, SchemaVersionPrivate)
	}
	if m.Private == nil || m.Private.Path != PrivatePath || !m.Private.Encrypted {
		t.Fatalf("private ref = %+v", m.Private)
	}
	if m.Sources != nil || m.Host != (HostInfo{}) || m.Totals != (Totals{}) {
		t.Fatalf("public manifest still describes the content: %+v", m)
	}
	if m.Encryption.KeyFingerprint != "" || m.Encryption.Recipients != nil {
		t.Fatalf("public manifest still identifies the key: %+v", m.Encryption)
	}
	if m.Encryption.NonceMode != "random" || m.Encryption.AEAD != "aes256-gcm" {
		t.Fatalf("public encryption settings must survive: %+v", m.Encryption)
	}
	for i, c := range table.Chunks {
		if c.Ps != "" || c.Pb != 0 {
			t.Fatalf("chunk %d still describes the plaintext: %+v", i, c)
		}
		if c.Ss == "" || c.Sb == 0 || c.P == "" {
			t.Fatalf("chunk %d lost the fields a keyless reader needs: %+v", i, c)
		}
	}

	// A stripped manifest and chunk table must still validate on their own.
	var mbuf, tbuf bytes.Buffer
	if err := WriteManifest(&mbuf, m); err != nil {
		t.Fatal(err)
	}
	if err := WriteChunkTable(&tbuf, table); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(mbuf.Bytes(), []byte("secret-project")) || bytes.Contains(mbuf.Bytes(), []byte("vault-01")) {
		t.Fatal("public manifest JSON leaks source or host")
	}
	if bytes.Contains(tbuf.Bytes(), []byte(strings.Repeat("11", 32))) {
		t.Fatal("public chunk table JSON leaks a plaintext digest")
	}
	readManifest, err := ReadManifest(bytes.NewReader(mbuf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	readTable, err := ReadChunkTable(bytes.NewReader(tbuf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	if err := MergePrivate(readManifest, readTable, private); err != nil {
		t.Fatal(err)
	}
	want, wantTable := encryptedManifest(), encryptedChunkTable()
	if readManifest.Sources[0] != want.Sources[0] || readManifest.Host != want.Host || readManifest.Totals != want.Totals {
		t.Fatalf("merged manifest = %+v", readManifest)
	}
	if readManifest.Encryption.KeyFingerprint != want.Encryption.KeyFingerprint {
		t.Fatalf("merged fingerprint = %q", readManifest.Encryption.KeyFingerprint)
	}
	for i, c := range readTable.Chunks {
		if c.Ps != wantTable.Chunks[i].Ps || c.Pb != wantTable.Chunks[i].Pb {
			t.Fatalf("merged chunk %d = %+v", i, c)
		}
	}
}

func TestSplitPrivateSkipsUnencryptedBackup(t *testing.T) {
	m, table := encryptedManifest(), encryptedChunkTable()
	m.Encryption = EncryptionInfo{}
	if private := SplitPrivate(m, table); private != nil {
		t.Fatal("an unencrypted backup must keep its public metadata")
	}
	if m.SchemaVersion != SchemaVersion || m.Private != nil || m.Sources == nil {
		t.Fatalf("manifest was modified: %+v", m)
	}
	if table.Chunks[0].Ps == "" || table.Chunks[0].Pb == 0 {
		t.Fatalf("chunk table was modified: %+v", table.Chunks[0])
	}
}

func TestPrivateBlobRoundTrip(t *testing.T) {
	sealer, opener, _ := testSealerOpener(t, crypt.NonceRandom)
	m, table := encryptedManifest(), encryptedChunkTable()
	private := SplitPrivate(m, table)
	var buf bytes.Buffer
	if err := WritePrivate(&buf, private, sealer); err != nil {
		t.Fatal(err)
	}
	if !crypt.IsEnvelope(buf.Bytes()) {
		t.Fatal("private blob is not an encrypted envelope")
	}
	if bytes.Contains(buf.Bytes(), []byte("secret-project")) {
		t.Fatal("private blob is not encrypted")
	}
	got, err := ReadPrivate(bytes.NewReader(buf.Bytes()), opener)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sources[0] != "/srv/secret-project" || got.Totals.Files != 7 || len(got.Chunks) != 2 {
		t.Fatalf("private = %+v", got)
	}
	if got.Chunks[1].Ps != "sha256:"+strings.Repeat("33", 32) || got.Chunks[1].Pb != 1096 {
		t.Fatalf("private chunk = %+v", got.Chunks[1])
	}
}

func TestPrivateBlobRejectsForeignKey(t *testing.T) {
	sealer, _, _ := testSealerOpener(t, crypt.NonceRandom)
	_, other, _ := testSealerOpener(t, crypt.NonceRandom)
	m, table := encryptedManifest(), encryptedChunkTable()
	var buf bytes.Buffer
	if err := WritePrivate(&buf, SplitPrivate(m, table), sealer); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(bytes.NewReader(buf.Bytes()), other); err == nil {
		t.Fatal("another key must not open the private blob")
	}
	if _, err := ReadPrivate(bytes.NewReader(buf.Bytes()), nil); err == nil {
		t.Fatal("a keyless reader must not open the private blob")
	}
}

func TestWritePrivateRequiresEncryption(t *testing.T) {
	m, table := encryptedManifest(), encryptedChunkTable()
	if err := WritePrivate(&bytes.Buffer{}, SplitPrivate(m, table), nil); err == nil {
		t.Fatal("private metadata must not be written in the clear")
	}
}

func TestMergePrivateRejectsMismatchedTable(t *testing.T) {
	m, table := encryptedManifest(), encryptedChunkTable()
	private := SplitPrivate(m, table)
	private.Chunks = private.Chunks[:1]
	if err := MergePrivate(m, table, private); err == nil {
		t.Fatal("a truncated private chunk list must be rejected")
	}
}

func TestChunkTableWithoutPlaintextFields(t *testing.T) {
	table := encryptedChunkTable()
	SplitPrivate(encryptedManifest(), table)
	var buf bytes.Buffer
	if err := WriteChunkTable(&buf, table); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"ps"`)) || bytes.Contains(buf.Bytes(), []byte(`"pb"`)) {
		t.Fatalf("stripped chunk table still carries plaintext fields: %s", buf.String())
	}
	if _, err := ReadChunkTable(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("a stripped chunk table must be readable: %v", err)
	}
}

// TestConvergentMetadataNonceIsContentDerived is the regression test for a
// nonce reuse: the metadata blobs used to be sealed with a constant plaintext
// digest, so two backups sharing a convergent repository key sealed different
// metadata under the same AES-GCM nonce.
func TestConvergentMetadataNonceIsContentDerived(t *testing.T) {
	sealer, opener, _ := testSealerOpener(t, crypt.NonceConvergent)

	seal := func(idx *Index) []byte {
		t.Helper()
		var buf bytes.Buffer
		if err := WriteIndex(&buf, idx, sealer); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	nonceOf := func(blob []byte) [12]byte {
		t.Helper()
		h, _, err := crypt.ParseHeader(blob)
		if err != nil {
			t.Fatal(err)
		}
		return h.Nonce
	}

	first := &Index{SchemaVersion: SchemaVersion, Entries: []FileEntry{{
		Path: "a.txt", Type: TypeRegular, Mode: "0644", SHA256: strings.Repeat("aa", 32),
	}}}
	second := &Index{SchemaVersion: SchemaVersion, Entries: []FileEntry{{
		Path: "b.txt", Type: TypeRegular, Mode: "0644", SHA256: strings.Repeat("bb", 32),
	}}}

	a, b := seal(first), seal(second)
	if nonceOf(a) == nonceOf(b) {
		t.Fatal("two different indexes were sealed under the same nonce")
	}
	// Determinism is what makes convergent dedup work: identical metadata must
	// still produce identical bytes.
	if !bytes.Equal(a, seal(first)) {
		t.Fatal("sealing the same index twice is not deterministic")
	}
	got, err := ReadIndex(bytes.NewReader(b), opener)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Path != "b.txt" {
		t.Fatalf("index = %+v", got.Entries)
	}

	m, table := encryptedManifest(), encryptedChunkTable()
	m.Encryption.NonceMode = "convergent"
	var privateBlob bytes.Buffer
	if err := WritePrivate(&privateBlob, SplitPrivate(m, table), sealer); err != nil {
		t.Fatal(err)
	}
	if nonceOf(privateBlob.Bytes()) == nonceOf(a) {
		t.Fatal("private blob and index were sealed under the same nonce")
	}
}
