package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
)

// TestMustVerify pins the rule: on an encrypted backup the per-chunk plaintext
// digest is checked whatever the caller asked for, because since schema 2 that
// digest lives in the sealed private blob and is the last link of the integrity
// chain. On a plaintext backup every digest is public anyway, so --no-verify
// keeps meaning what it always meant.
func TestMustVerify(t *testing.T) {
	cases := []struct {
		name      string
		encrypted bool
		asked     bool
		want      bool
	}{
		{"encrypted, verify asked", true, true, true},
		{"encrypted, no-verify asked", true, false, true},
		{"plaintext, verify asked", false, true, true},
		{"plaintext, no-verify asked", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &Backup{Manifest: &index.Manifest{
				Encryption: index.EncryptionInfo{Enabled: c.encrypted},
			}}
			if got := b.mustVerify(c.asked); got != c.want {
				t.Fatalf("mustVerify(%v) = %v, want %v", c.asked, got, c.want)
			}
		})
	}
}

// TestNoVerifyStillCatchesForgedChunk is the end-to-end half of the same rule.
//
// The forged blob below authenticates perfectly: it is sealed with the real
// key, at the real chunk index, and its stored digest in chunks.json is fixed
// up the way anybody rewriting that public file would. Its plaintext is a
// different string of the same length, so neither AES-GCM nor the size check
// notices. Only the plaintext digest from the sealed private blob does — and it
// must do so even with verify=false, or --no-verify would be a switch that
// turns off the last defence of an encrypted backup.
func TestNoVerifyStillCatchesForgedChunk(t *testing.T) {
	ctx := context.Background()
	f := makePrivateFixture(t, 1024)
	if f.chunkCount < 2 {
		t.Fatalf("fixture must have several chunks, got %d", f.chunkCount)
	}

	// The key ships inside the backup, so an attacker holding the image plus
	// the passphrase can forge; that is the scenario a shared repository key
	// creates for every other backup in the repository.
	keyFile, err := os.Open(filepath.Join(f.root, "keys.pass.age"))
	if err != nil {
		t.Fatal(err)
	}
	km, err := crypt.UnwrapKeys(keyFile, crypt.Identity{Passphrase: []byte(fixturePassphrase)})
	keyFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()

	table := readTable(t, filepath.Join(f.root, "chunks.json"))
	first := table.Chunks[0]

	codec, err := compress.Get("store")
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := crypt.NewSealer(km, crypt.NonceRandom)
	if err != nil {
		t.Fatal(err)
	}
	// chunks.json of a schema 2 backup carries no plaintext size — that is the
	// point of the private blob — so recover it from the stored size, which is
	// the payload plus a fixed envelope overhead under the `store` codec.
	plainBytes := first.Sb - int64(sealer.Overhead())
	if plainBytes <= 0 {
		t.Fatalf("cannot derive the plaintext size from Sb=%d", first.Sb)
	}
	forgedPlain := bytes.Repeat([]byte("X"), int(plainBytes))
	forged, err := sealer.Seal(nil, crypt.RoleData, 0, codec, forgedPlain)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(forged)) != first.Sb {
		t.Fatalf("forged blob is %d bytes, original %d: the test needs them equal", len(forged), first.Sb)
	}

	blobs, err := os.ReadFile(f.chunkPath)
	if err != nil {
		t.Fatal(err)
	}
	copy(blobs[:len(forged)], forged)
	if err := os.WriteFile(f.chunkPath, blobs, 0o600); err != nil {
		t.Fatal(err)
	}
	// chunks.json is public and unauthenticated: an attacker fixes it up too.
	sum := sha256.Sum256(forged)
	table.Chunks[0].Ss = "sha256:" + hex.EncodeToString(sum[:])
	writeFile(t, filepath.Join(f.root, "chunks.json"), func(w io.Writer) error {
		return index.WriteChunkTable(w, table)
	})

	b, err := OpenLocal(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Unlock(ctx, crypt.Identity{Passphrase: []byte(fixturePassphrase)}); err != nil {
		t.Fatal(err)
	}

	// The forgery survives the keyless checks: that is exactly why the
	// plaintext digest may not be optional.
	quick, err := b.Verify(ctx, false, false)
	if err != nil || !quick.OK {
		t.Fatalf("forged blob must pass the keyless quick verify: %+v %v", quick, err)
	}

	// verify=false must not help it through.
	err = b.StreamTar(ctx, io.Discard, false)
	if !errors.Is(err, crypt.ErrIntegrity) {
		t.Fatalf("StreamTar(verify=false) = %v, want ErrIntegrity", err)
	}

	idx, err := b.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = b.StreamSelectedTar(ctx, idx, idx.Entries, io.Discard, false)
	if !errors.Is(err, crypt.ErrIntegrity) {
		t.Fatalf("StreamSelectedTar(verify=false) = %v, want ErrIntegrity", err)
	}
}

func readTable(t *testing.T, path string) *index.ChunkTable {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	table, err := index.ReadChunkTable(f)
	if err != nil {
		t.Fatal(err)
	}
	return table
}
