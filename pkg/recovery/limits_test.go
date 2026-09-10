package recovery

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/manprint/backimage/pkg/index"
)

// allocatedBy measures what fn allocates. The point of A7.1 is not only that
// a hostile size is refused: it is that it is refused *before* the reader
// tries to hold it, so the measurement is the assertion.
func allocatedBy(t *testing.T, fn func()) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// hostileSize is what the rewritten metadata claims. It is far past anything
// a machine running the tests would hand out quietly.
const hostileSize = 8 << 30

// TestAStoredSizeTheManifestDoesNotAgreeWithIsRefusedAtOpen is the first
// half: chunks.json alone declares the absurd size, so the manifest already
// contradicts it and the backup never opens.
func TestAStoredSizeTheManifestDoesNotAgreeWithIsRefusedAtOpen(t *testing.T) {
	f := buildFixture(t, false, 1024, false)
	table := readTable(t, filepath.Join(f.root, "chunks.json"))
	table.Chunks[0].Sb = hostileSize
	writeFile(t, filepath.Join(f.root, "chunks.json"), func(w io.Writer) error {
		return index.WriteChunkTable(w, table)
	})

	var err error
	allocated := allocatedBy(t, func() {
		var b *Backup
		b, err = OpenLocal(context.Background(), f.root)
		if b != nil {
			_ = b.Close()
		}
	})
	if allocated > 4<<20 {
		t.Fatalf("refusing a %d byte declaration allocated %d bytes", int64(hostileSize), allocated)
	}
	if !errors.Is(err, index.ErrBadSchema) {
		t.Fatalf("OpenLocal = %v, want ErrBadSchema", err)
	}
}

// TestAStoredSizeTheWholeMetadataAgreesOnIsRefusedByTheBlob is the second
// half, and the reason the file size is consulted at all: manifest.json and
// chunks.json are both public, so an attacker can make them agree. The blob
// on disk cannot be talked into being 8 GiB.
func TestAStoredSizeTheWholeMetadataAgreesOnIsRefusedByTheBlob(t *testing.T) {
	f := buildFixture(t, false, 1024, false)

	table := readTable(t, filepath.Join(f.root, "chunks.json"))
	// One chunk, one layer, one blob: every public number agrees with every
	// other one, exactly as a careful rewrite would leave them.
	table.Chunks = table.Chunks[:1]
	table.Chunks[0].Sb = hostileSize
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
	m.Chunking.Count = 1
	m.Chunking.TargetChunkBytes = hostileSize
	m.Layers = []index.LayerInfo{{
		Index: 0, Digest: "sha256:fixture", ChunkFrom: 0, ChunkTo: 0, StoredBytes: hostileSize,
	}}
	writeFile(t, filepath.Join(f.root, "manifest.json"), func(w io.Writer) error {
		return index.WriteManifest(w, m)
	})

	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatalf("a self-consistent rewrite must still open: %v", err)
	}
	defer b.Close()

	allocated := allocatedBy(t, func() {
		_, err = b.StoredChunk(context.Background(), 0)
	})
	if allocated > 4<<20 {
		t.Fatalf("refusing a %d byte declaration allocated %d bytes", int64(hostileSize), allocated)
	}
	if !errors.Is(err, index.ErrBadSchema) {
		t.Fatalf("StoredChunk = %v, want ErrBadSchema", err)
	}
}

// TestTheHonestFixtureStillReads is the control both tests above need: the
// same reader, the same helper, nothing rewritten.
func TestTheHonestFixtureStillReads(t *testing.T) {
	f := buildFixture(t, false, 1024, false)
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for i := 0; i < f.chunkCount; i++ {
		if _, err := b.StoredChunk(context.Background(), i); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
}
