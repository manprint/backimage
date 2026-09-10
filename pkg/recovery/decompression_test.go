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
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/manprint/backimage/pkg/index"
)

// zstdBomb compresses n zero bytes. A few hundred bytes of blob produce
// whatever the reader is willing to take.
func zstdBomb(t *testing.T, n int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, io.LimitReader(zeroSource{}, n)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zeroSource struct{}

func (zeroSource) Read(p []byte) (int, error) { return len(p), nil }

// bombFixture rewrites an unencrypted fixture so that its single chunk is a
// decompression bomb whose declared plaintext size is small. Every public
// number agrees; only the frame disagrees, and it does so while expanding.
func bombFixture(t *testing.T, plainDeclared int64, bombBytes int64) *Backup {
	t.Helper()
	f := buildFixture(t, false, 1024, false)
	bomb := zstdBomb(t, bombBytes)
	if len(bomb) > 1<<20 {
		t.Fatalf("the fixture must be cheap to publish, it is %d bytes", len(bomb))
	}
	if err := os.WriteFile(f.chunkPath, bomb, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bomb)
	table := readTable(t, filepath.Join(f.root, "chunks.json"))
	table.Chunks = table.Chunks[:1]
	table.Chunks[0].Sb = int64(len(bomb))
	table.Chunks[0].Ss = "sha256:" + hex.EncodeToString(sum[:])
	table.Chunks[0].Pb = plainDeclared
	writeFile(t, filepath.Join(f.root, "chunks.json"), func(w io.Writer) error {
		return index.WriteChunkTable(w, table)
	})

	file, err := os.Open(filepath.Join(f.root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := index.ReadManifest(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	m.Archive.Compression = "zstd"
	m.Chunking.Count = 1
	m.Chunking.TargetChunkBytes = plainDeclared
	m.Layers = []index.LayerInfo{{
		Index: 0, Digest: "sha256:fixture", ChunkFrom: 0, ChunkTo: 0, StoredBytes: int64(len(bomb)),
	}}
	writeFile(t, filepath.Join(f.root, "manifest.json"), func(w io.Writer) error {
		return index.WriteManifest(w, m)
	})

	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatalf("a self-consistent rewrite must open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestAChunkCannotDecompressPastTheSizeItDeclares is A7.3 where the backup
// itself says what the answer should be: the plaintext size of a chunk is
// recorded when the backup is made. A frame that keeps producing bytes past
// it is a bomb, and the reader stops at the declared size instead of finding
// out afterwards.
func TestAChunkCannotDecompressPastTheSizeItDeclares(t *testing.T) {
	b := bombFixture(t, 1<<20, 512<<20)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r, openErr := b.PlainChunk(context.Background(), 0)
	if openErr != nil {
		t.Fatal(openErr)
	}
	produced, err := io.Copy(io.Discard, r)
	closeErr := r.Close()
	runtime.ReadMemStats(&after)

	// Not one byte past what the backup said this chunk holds: whoever is
	// reading is often writing it somewhere, and a refusal that emits part of
	// what it refuses is not a refusal.
	if produced != 1<<20 {
		t.Fatalf("the bomb produced %d bytes for a chunk declared as %d", produced, 1<<20)
	}

	// The decoder's own buffers are a few megabytes whatever the frame says;
	// what must not appear here is the half gigabyte the frame produces.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("stopping a %d byte bomb allocated %d bytes", 512<<20, allocated)
	}
	if !errors.Is(err, index.ErrBadSchema) {
		t.Fatalf("reading the chunk = %v (close %v), want ErrBadSchema", err, closeErr)
	}
}

// TestAnHonestChunkDecompressesWhole is the control: the cap is the declared
// size, so a chunk that produces exactly that must not be truncated.
func TestAnHonestChunkDecompressesWhole(t *testing.T) {
	const size = 4 << 20
	b := bombFixture(t, size, size)
	r, err := b.PlainChunk(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, r)
	if closeErr := r.Close(); err == nil {
		err = closeErr
	}
	if err != nil || n != size {
		t.Fatalf("an honest chunk read %d bytes, %v", n, err)
	}
}
