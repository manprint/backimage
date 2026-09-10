package recovery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
)

// unlockFixture opens a fixture directory and unlocks it, returning the error
// instead of failing: every test here is about which error comes out.
func unlockFixture(t *testing.T, root string) error {
	t.Helper()
	b, err := OpenLocal(context.Background(), root)
	if err != nil {
		return err
	}
	t.Cleanup(func() { _ = b.Close() })
	return b.Unlock(context.Background(), crypt.Identity{Passphrase: []byte(fixturePassphrase)})
}

// TestTheSealedBindingCatchesARepairedChunkTable is A20. chunks.json is
// public and carries no signature, so an attacker who can write the image can
// edit it and fix up every number that describes the blobs. The only thing
// that ever noticed was the chunk count. Now the sealed metadata names the
// chunk table that belongs to this backup, and the mismatch is refused before
// a reader parses anything.
func TestTheSealedBindingCatchesARepairedChunkTable(t *testing.T) {
	f := buildFixture(t, true, 1024, true)
	table := readTable(t, filepath.Join(f.root, "chunks.json"))
	// A repair that keeps every count and every size consistent: only the
	// stored digest of one chunk moves, exactly as a substitution would.
	table.Chunks[0].Ss = "sha256:" + "00" + table.Chunks[0].Ss[len("sha256:")+2:]
	writeFile(t, filepath.Join(f.root, "chunks.json"), func(w io.Writer) error {
		return index.WriteChunkTable(w, table)
	})

	err := unlockFixture(t, f.root)
	if !errors.Is(err, index.ErrBadSchema) {
		t.Fatalf("unlock = %v, want the binding to refuse the edited chunk table", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("chunks.json")) {
		t.Fatalf("the error must name the file that does not match: %v", err)
	}
}

// TestTheSealedBindingCatchesARewrittenManifest covers the other public file.
// The fields that decide how a backup is read — schema, aead, nonce mode,
// envelope version, whether the index is sealed — all live there.
func TestTheSealedBindingCatchesARewrittenManifest(t *testing.T) {
	cases := map[string]func(*index.Manifest){
		"nonce mode":       func(m *index.Manifest) { m.Encryption.NonceMode = "convergent" },
		"envelope version": func(m *index.Manifest) { m.Encryption.EnvelopeVersion = 1 },
		"aead":             func(m *index.Manifest) { m.Encryption.AEAD = "chacha20-poly1305" },
		"created at":       func(m *index.Manifest) { m.CreatedAt = m.CreatedAt.Add(1) },
		"layer digest":     func(m *index.Manifest) { m.Layers[0].Digest = "sha256:another" },
		"chunk size":       func(m *index.Manifest) { m.Chunking.TargetChunkBytes *= 2 },
	}
	for name, rewrite := range cases {
		t.Run(name, func(t *testing.T) {
			f := buildFixture(t, true, 1024, true)
			file, err := os.Open(filepath.Join(f.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			m, err := index.ReadManifest(file)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			rewrite(m)
			writeFile(t, filepath.Join(f.root, "manifest.json"), func(w io.Writer) error {
				return index.WriteManifest(w, m)
			})
			if err := unlockFixture(t, f.root); !errors.Is(err, index.ErrBadSchema) {
				t.Fatalf("unlock = %v, want the binding to refuse the rewritten manifest", err)
			}
		})
	}
}

// TestMetadataDoesNotTravelBetweenBackups is the composition the review
// described: two backups sealed with one repository key, with matching counts,
// and the metadata of one served with the data of the other.
func TestMetadataDoesNotTravelBetweenBackups(t *testing.T) {
	victim := buildFixture(t, true, 1024, true)
	other := buildFixture(t, true, 1024, true)

	// Both fixtures are built by the same helper, so their chunk counts and
	// shapes agree — which is all the old cross-check ever verified. Their
	// manifests are byte-identical for the same reason, so swapping that one
	// is not a swap at all; a manifest that says something different is what
	// TestTheSealedBindingCatchesARewrittenManifest covers.
	for _, name := range []string{"chunks.json", index.PrivatePath} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			copyFixtureTree(t, victim.root, root)
			data, err := os.ReadFile(filepath.Join(other.root, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
			// The two fixtures do not share a key, so a swapped private blob
			// fails to open; the other two fail on the binding. Either way
			// nothing is delivered.
			if err := unlockFixture(t, root); err == nil {
				t.Fatal("a backup composed from two different ones must not open")
			}
		})
	}
}

// TestAnIndexFromAnotherBackupIsRefused covers the file the manifest never
// carried a digest for. It is fetched later than the rest, so it is checked
// where it is read.
func TestAnIndexFromAnotherBackupIsRefused(t *testing.T) {
	victim := buildFixture(t, true, 1024, true)
	other := buildFixture(t, true, 1024, true)

	root := t.TempDir()
	copyFixtureTree(t, victim.root, root)
	// The index of the other backup, sealed under the victim's own key so
	// that the envelope opens and only the binding can tell them apart.
	otherIndex := reindexUnderKey(t, other, victim.km)
	if err := os.WriteFile(filepath.Join(root, "index.json.zst"), otherIndex, 0o600); err != nil {
		t.Fatal(err)
	}

	b, err := OpenLocal(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Unlock(context.Background(), crypt.Identity{Passphrase: []byte(fixturePassphrase)}); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := b.Index(context.Background()); !errors.Is(err, index.ErrBadSchema) {
		t.Fatalf("Index = %v, want the binding to refuse an index from another backup", err)
	}
}

// TestAPrivateBlobWithoutABindingIsRefusedUnderACurrentKey states the rule
// that keeps the property from being removable. The binding lives in the
// sealed blob, so it cannot be edited — but an older sealed blob, from before
// the binding existed, could be served in its place. The key material says
// which release wrote the backup, and that is authenticated too.
func TestAPrivateBlobWithoutABindingIsRefusedUnderACurrentKey(t *testing.T) {
	f := buildFixture(t, true, 1024, true)
	opener, err := crypt.NewKeyedOpener(f.km)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(f.root, index.PrivatePath))
	if err != nil {
		t.Fatal(err)
	}
	private, err := index.ReadPrivate(bytes.NewReader(blob), opener)
	if err != nil {
		t.Fatal(err)
	}
	private.Binding = nil
	sealer, err := crypt.NewSealer(f.km, crypt.NonceRandom)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.root, index.PrivatePath), func(w io.Writer) error {
		return index.WritePrivate(w, private, sealer)
	})

	if err := unlockFixture(t, f.root); !errors.Is(err, index.ErrBadSchema) {
		t.Fatalf("unlock = %v, want a current key to require a binding", err)
	}
}

// copyFixtureTree duplicates a fixture directory so a test can damage the
// copy.
func copyFixtureTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// reindexUnderKey re-seals the file index of f with km, so the resulting blob
// opens under that key and differs only in what it says.
func reindexUnderKey(t *testing.T, f fixture, km *crypt.KeyMaterial) []byte {
	t.Helper()
	sealer, err := crypt.NewSealer(km, crypt.NonceRandom)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	entries := append([]index.FileEntry(nil), f.entries...)
	entries[0].Path = entries[0].Path + "-from-another-backup"
	if err := index.WriteIndex(&buf, &index.Index{
		SchemaVersion: index.SchemaVersionPrivate, Entries: entries,
	}, sealer); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
