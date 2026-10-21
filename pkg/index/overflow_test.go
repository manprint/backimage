package index

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// The chunks of a layer were summed without an overflow check: MaxInt64 +
// MaxInt64 + MaxInt64 + 2 wraps to exactly MaxInt64, and a table whose
// chunks add up to three times their layer validated as that layer.
func TestLayerSumCannotWrapBackToItsSize(t *testing.T) {
	m := limitsManifest()
	m.Chunking = ChunkingInfo{Strategy: "length", Count: 4}
	m.Layers = []LayerInfo{{Index: 0, Digest: "sha256:layer", ChunkFrom: 0, ChunkTo: 3, StoredBytes: math.MaxInt64}}
	tab := &ChunkTable{SchemaVersion: SchemaVersion}
	for i, sb := range []int64{math.MaxInt64, math.MaxInt64, math.MaxInt64, 2} {
		tab.Chunks = append(tab.Chunks, Chunk{I: i, P: "backup/data/a.blob", Ss: "sha256:aa", Sb: sb})
	}
	if err := ValidateChunkTable(m, tab); !errors.Is(err, ErrBadSchema) {
		t.Fatalf("chunks summing past their layer through an overflow = %v, want ErrBadSchema", err)
	}
}

// plain + plain/64 + 4096 overflowed for a huge declared chunk size: the
// bound came out negative and the per-chunk check was skipped.
func TestMaxStoredChunkBytesSaturates(t *testing.T) {
	for _, plain := range []int64{math.MaxInt64, math.MaxInt64 - 4096, math.MaxInt64 / 64 * 63} {
		m := limitsManifest()
		m.Chunking.TargetChunkBytes = plain
		if got := MaxStoredChunkBytes(m); got < plain {
			t.Errorf("MaxStoredChunkBytes(target %d) = %d, below the chunk size itself", plain, got)
		}
	}
	m := limitsManifest()
	if got := MaxStoredChunkBytes(m); got != 1024+1024/64+4096 {
		t.Errorf("MaxStoredChunkBytes(1024) = %d", got)
	}
}

// The plain sizes were summed without a check either: three chunks of
// MaxInt64, MaxInt64 and 3 bytes made a 1-byte stream whose offset 0 sat in
// the first chunk.
func TestLocatorRefusesSizesThatOverflow(t *testing.T) {
	tab := &ChunkTable{SchemaVersion: SchemaVersion, Chunks: []Chunk{
		{I: 0, Pb: math.MaxInt64}, {I: 1, Pb: math.MaxInt64}, {I: 2, Pb: 3},
	}}
	if i, off, err := NewLocator(tab).ChunkForOffset(0); err == nil {
		t.Fatalf("an overflowing table located offset 0 in chunk %d at %d", i, off)
	}
}

// A size near MaxInt64 made the span of an entry wrap. Range refused the
// result, but under a message about a range nobody asked for.
func TestEntrySpanOverflowIsNamed(t *testing.T) {
	l := NewLocator(&ChunkTable{SchemaVersion: SchemaVersion, Chunks: []Chunk{{I: 0, Pb: 4096}}})
	for _, e := range []FileEntry{
		{Path: "big", TarOffset: 0, Size: math.MaxInt64},
		{Path: "far", TarOffset: math.MaxInt64 - 100, Size: 1},
		{Path: "neg", TarOffset: 0, Size: -1},
	} {
		_, err := ChunksFor(l, []FileEntry{e})
		if err == nil || !strings.Contains(err.Error(), "overflow") && !strings.Contains(err.Error(), "negative") {
			t.Errorf("ChunksFor(%s) = %v, want the span refused by name", e.Path, err)
		}
	}
	if chunks, err := ChunksFor(l, []FileEntry{{Path: "ok", TarOffset: 0, Size: 100}}); err != nil || len(chunks) != 1 {
		t.Fatalf("ChunksFor(ok) = %v, %v", chunks, err)
	}
}
