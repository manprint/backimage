package index

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func bindingManifest() *Manifest {
	return &Manifest{
		SchemaVersion: SchemaVersionPrivate,
		Tool:          ToolInfo{Name: "backimage", Version: "test"},
		CreatedAt:     time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Archive:       ArchiveInfo{Format: "tar", Compression: "zstd", CompressionLevel: 3},
		Encryption: EncryptionInfo{
			Enabled: true, KDF: "scrypt-age", AEAD: "aes256-gcm",
			EnvelopeVersion: 3, NonceMode: "random",
		},
		Chunking: ChunkingInfo{Strategy: "length", TargetChunkBytes: 1024, Count: 2},
		Layers:   []LayerInfo{{Index: 0, Digest: "sha256:layer", ChunkFrom: 0, ChunkTo: 1, StoredBytes: 2048}},
		Index:    Ref{Path: "index.json.zst", StoredSha256: "", Encrypted: true},
		Private:  &Ref{Path: PrivatePath, StoredSha256: "sha256:private", Encrypted: true},
	}
}

func bindingTable() *ChunkTable {
	return &ChunkTable{SchemaVersion: SchemaVersionPrivate, Chunks: []Chunk{
		{I: 0, P: "data/0.blob", Ss: "sha256:aa", Sb: 1024},
		{I: 1, P: "data/0.blob", Ss: "sha256:bb", Sb: 1024},
	}}
}

// TestTheBindingIgnoresWhatTheUnlockRestores is what makes the check usable
// at all: the reader verifies before MergePrivate and the writer computes
// after SplitPrivate, so the digest must not move when the confidential
// fields come back.
func TestTheBindingIgnoresWhatTheUnlockRestores(t *testing.T) {
	before, err := ManifestBindingDigest(bindingManifest())
	if err != nil {
		t.Fatal(err)
	}
	merged := bindingManifest()
	merged.Sources = []string{"/srv/data"}
	merged.Host = HostInfo{Hostname: "host", OS: "linux"}
	merged.Totals = Totals{Files: 3}
	merged.Encryption.KeyFingerprint = "0123456789abcdef"
	merged.Encryption.Recipients = []string{"age1example"}
	after, err := ManifestBindingDigest(merged)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("the manifest digest moved when the private fields were merged back")
	}

	// Same for the chunk table: the plaintext digests arrive at merge time.
	plain, err := ChunkTableBindingDigest(bindingTable())
	if err != nil {
		t.Fatal(err)
	}
	withSecrets := bindingTable()
	withSecrets.Chunks[0].Ps, withSecrets.Chunks[0].Pb = "sha256:cc", 900
	got, err := ChunkTableBindingDigest(withSecrets)
	if err != nil {
		t.Fatal(err)
	}
	if plain != got {
		t.Fatal("the chunk table digest moved when the plaintext digests were merged back")
	}
}

// TestTheBindingExcludesThePrivateReference is the cycle: the manifest names
// the digest of the private blob, and the private blob names the digest of
// the manifest. One of the two has to give.
func TestTheBindingExcludesThePrivateReference(t *testing.T) {
	m := bindingManifest()
	before, err := ManifestBindingDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	m.Private.StoredSha256 = "sha256:computed-after-the-fact"
	after, err := ManifestBindingDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("the private reference must be outside the digest, or it could never be filled in")
	}
}

// TestTheBindingCatchesEveryPublicEdit walks the fields a reader acts on.
func TestTheBindingCatchesEveryPublicEdit(t *testing.T) {
	binding, err := NewBinding(bindingManifest(), bindingTable(), []byte("index blob"))
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.Check(bindingManifest(), bindingTable()); err != nil {
		t.Fatalf("an untouched backup must pass: %v", err)
	}

	manifestEdits := map[string]func(*Manifest){
		"schema":           func(m *Manifest) { m.SchemaVersion = SchemaVersion },
		"aead":             func(m *Manifest) { m.Encryption.AEAD = "chacha20-poly1305" },
		"envelope version": func(m *Manifest) { m.Encryption.EnvelopeVersion = 2 },
		"nonce mode":       func(m *Manifest) { m.Encryption.NonceMode = "convergent" },
		"index encrypted":  func(m *Manifest) { m.Index.Encrypted = false },
		"layer digest":     func(m *Manifest) { m.Layers[0].Digest = "sha256:other" },
		"tool version":     func(m *Manifest) { m.Tool.Version = "0.0.0" },
		"created at":       func(m *Manifest) { m.CreatedAt = m.CreatedAt.Add(time.Second) },
		"compression":      func(m *Manifest) { m.Archive.Compression = "gzip" },
	}
	for name, edit := range manifestEdits {
		t.Run("manifest "+name, func(t *testing.T) {
			m := bindingManifest()
			edit(m)
			if err := binding.Check(m, bindingTable()); !errors.Is(err, ErrBadSchema) {
				t.Fatalf("Check = %v, want ErrBadSchema", err)
			}
		})
	}

	tableEdits := map[string]func(*ChunkTable){
		"stored digest": func(t *ChunkTable) { t.Chunks[0].Ss = "sha256:zz" },
		"stored size":   func(t *ChunkTable) { t.Chunks[1].Sb = 999 },
		"blob path":     func(t *ChunkTable) { t.Chunks[1].P = "data/1.blob" },
		"chunk removed": func(t *ChunkTable) { t.Chunks = t.Chunks[:1] },
	}
	for name, edit := range tableEdits {
		t.Run("chunks "+name, func(t *testing.T) {
			table := bindingTable()
			edit(table)
			if err := binding.Check(bindingManifest(), table); !errors.Is(err, ErrBadSchema) {
				t.Fatalf("Check = %v, want ErrBadSchema", err)
			}
		})
	}

	t.Run("index blob", func(t *testing.T) {
		if err := binding.CheckIndexBlob([]byte("another index blob")); !errors.Is(err, ErrBadSchema) {
			t.Fatalf("CheckIndexBlob = %v, want ErrBadSchema", err)
		}
		if err := binding.CheckIndexBlob([]byte("index blob")); err != nil {
			t.Fatalf("the right index blob must pass: %v", err)
		}
	})
}

// TestPolicyIsCheckedBeforeTheDigests keeps the order the plan asked for:
// what the backup claims to be first, what it contains second. A reader that
// believed the digests of a manifest whose policy it had not checked would
// have validated the wrong shape carefully.
func TestPolicyIsCheckedBeforeTheDigests(t *testing.T) {
	binding, err := NewBinding(bindingManifest(), bindingTable(), []byte("index blob"))
	if err != nil {
		t.Fatal(err)
	}
	m := bindingManifest()
	m.Encryption.NonceMode = "convergent"
	m.Layers[0].Digest = "sha256:also-wrong"
	err = binding.Check(m, bindingTable())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if got := err.Error(); !strings.Contains(got, "nonceMode") {
		t.Fatalf("the policy must be reported first, got %q", got)
	}
}

// TestABindingIsRequiredToCheckAnything states that a nil binding never
// silently approves.
func TestABindingIsRequiredToCheckAnything(t *testing.T) {
	var binding *Binding
	if err := binding.Check(bindingManifest(), bindingTable()); !errors.Is(err, ErrBadSchema) {
		t.Fatalf("Check on a nil binding = %v, want ErrBadSchema", err)
	}
	// The index blob is the exception: a backup written before the binding
	// existed has nothing to compare it with, and the caller has already
	// decided whether that is acceptable.
	if err := binding.CheckIndexBlob([]byte("anything")); err != nil {
		t.Fatalf("CheckIndexBlob on a nil binding = %v, want nil", err)
	}
}
