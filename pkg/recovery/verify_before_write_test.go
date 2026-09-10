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

// TestStreamTarWritesNothingFromARejectedChunk pins the guarantee that a chunk
// which fails its plaintext digest contributes zero bytes to the output.
//
// The forgery is the realistic one: a blob sealed with the backup's own key,
// of exactly the original stored size, with its public stored digest fixed up
// the way anybody rewriting chunks.json would. Nothing keyless notices it;
// only the plaintext digest kept in the sealed private blob does.
//
// Putting it at chunk 1 rather than chunk 0 is what makes the measurement
// possible: chunk 0 is legitimate and must be written in full, so the byte
// count after the refusal says exactly whether any of chunk 1 leaked. Before
// verify-before-write it did — the whole chunk did, because the digest was
// only compared once io.Copy had finished feeding dst.
func TestStreamTarWritesNothingFromARejectedChunk(t *testing.T) {
	ctx := context.Background()
	f := makePrivateFixture(t, 1024)
	if f.chunkCount < 3 {
		t.Fatalf("fixture must have at least three chunks, got %d", f.chunkCount)
	}

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

	codec, err := compress.Get("store")
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := crypt.NewSealer(km, crypt.NonceRandom)
	if err != nil {
		t.Fatal(err)
	}
	overhead := int64(sealer.Overhead())

	const target = 1
	table := readTable(t, filepath.Join(f.root, "chunks.json"))
	victim := table.Chunks[target]
	plainBytes := victim.Sb - overhead
	if plainBytes <= 0 {
		t.Fatalf("cannot derive the plaintext size from Sb=%d", victim.Sb)
	}
	forged, err := sealer.Seal(nil, crypt.RoleData, target, codec, bytes.Repeat([]byte("X"), int(plainBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(forged)) != victim.Sb {
		t.Fatalf("forged blob is %d bytes, original %d: the test needs them equal", len(forged), victim.Sb)
	}

	var offset int64
	for _, c := range table.Chunks[:target] {
		if c.P == victim.P {
			offset += c.Sb
		}
	}
	blobs, err := os.ReadFile(f.chunkPath)
	if err != nil {
		t.Fatal(err)
	}
	copy(blobs[offset:offset+int64(len(forged))], forged)
	if err := os.WriteFile(f.chunkPath, blobs, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(forged)
	table.Chunks[target].Ss = "sha256:" + hex.EncodeToString(sum[:])
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

	// countingWriter (downgrade_test.go) records how much of the
	// reconstructed tar actually reached the consumer, which is the only
	// thing this test is about.
	var got countingWriter
	if err := b.StreamTar(ctx, &got, true); !errors.Is(err, crypt.ErrIntegrity) {
		t.Fatalf("StreamTar = %v, want ErrIntegrity", err)
	}
	want := int(table.Chunks[0].Sb - overhead)
	if got.n != want {
		t.Fatalf("the consumer received %d bytes, want exactly the %d of the intact first chunk", got.n, want)
	}
}

// TestPlainChunkStillOwnsItsBuffer guards the split between the payload
// accessor and the public reader: PlainChunk keeps wiping the compressed
// bytes on Close, so callers outside this package see no change.
func TestPlainChunkStillOwnsItsBuffer(t *testing.T) {
	ctx := context.Background()
	f := makeFixture(t, false, 1024)
	b, err := OpenLocal(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	r, err := b.PlainChunk(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	buffered, ok := r.(*bufferedReader)
	if !ok {
		t.Fatalf("PlainChunk returned %T, want *bufferedReader", r)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if buffered.data != nil {
		t.Fatal("Close left the compressed payload behind")
	}
}
