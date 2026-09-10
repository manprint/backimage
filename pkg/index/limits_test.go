package index

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// limitsManifest and limitsTable are one honest backup: two chunks in one
// layer, the layer size the sum of the two.
func limitsManifest() *Manifest {
	return &Manifest{
		SchemaVersion: SchemaVersion,
		Tool:          ToolInfo{Name: "backimage", Version: "test"},
		CreatedAt:     time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Archive:       ArchiveInfo{Format: "tar", Compression: "zstd"},
		Chunking:      ChunkingInfo{Strategy: "length", TargetChunkBytes: 1024, Count: 2},
		Layers: []LayerInfo{
			{Index: 0, Digest: "sha256:layer", ChunkFrom: 0, ChunkTo: 1, StoredBytes: 1500},
		},
		Index: Ref{Path: "index.json.zst"},
	}
}

func limitsTable() *ChunkTable {
	return &ChunkTable{SchemaVersion: SchemaVersion, Chunks: []Chunk{
		{I: 0, P: "backup/data/a.blob", Ss: "sha256:aa", Sb: 1000},
		{I: 1, P: "backup/data/a.blob", Ss: "sha256:bb", Sb: 500},
	}}
}

// TestAnHonestTableIsAccepted is the control: every rule below must be a rule
// the writer already satisfies.
func TestAnHonestTableIsAccepted(t *testing.T) {
	if err := ValidateChunkTable(limitsManifest(), limitsTable()); err != nil {
		t.Fatalf("an honest backup must validate: %v", err)
	}
}

// TestNoAllocationIsDecidedByAnUncheckedField walks the ways chunks.json can
// declare a size nobody verified. Each of these used to reach make([]byte,
// Sb) with the maximum integer as its only guard.
func TestNoAllocationIsDecidedByAnUncheckedField(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Manifest, *ChunkTable)
		says   string
	}{
		"a chunk larger than its layer": {
			func(_ *Manifest, tab *ChunkTable) { tab.Chunks[0].Sb = 100 << 30 },
			"more than the 1500 of layer 0",
		},
		"a chunk larger than any chunk of this backup": {
			func(m *Manifest, tab *ChunkTable) {
				m.Layers[0].StoredBytes = 100 << 30
				tab.Chunks[0].Sb = 100<<30 - 500
			},
			"a chunk of this backup can hold",
		},
		"the chunks of a layer do not add up": {
			func(_ *Manifest, tab *ChunkTable) { tab.Chunks[1].Sb = 400 },
			"add up to 1400",
		},
		"a chunk declaring no bytes": {
			func(_ *Manifest, tab *ChunkTable) { tab.Chunks[1].Sb = 0 },
			"declares 0 stored bytes",
		},
		"a negative stored size": {
			func(_ *Manifest, tab *ChunkTable) { tab.Chunks[1].Sb = -1 },
			"declares -1 stored bytes",
		},
		"a layer declaring no bytes": {
			func(m *Manifest, _ *ChunkTable) { m.Layers[0].StoredBytes = 0 },
			"layer 0 declares 0 stored bytes",
		},
		"a chunk that is not where it says it is": {
			func(_ *Manifest, tab *ChunkTable) { tab.Chunks[1].I = 7 },
			"says it is chunk 7",
		},
		"a chunk in no layer at all": {
			func(m *Manifest, _ *ChunkTable) { m.Layers[0].ChunkTo = 0; m.Layers[0].StoredBytes = 1000 },
			"belong to no layer",
		},
		"a layer that skips a chunk": {
			func(m *Manifest, _ *ChunkTable) { m.Layers[0].ChunkFrom = 1; m.Layers[0].StoredBytes = 500 },
			"starts at chunk 1",
		},
		"a chunk reading another layer's blob": {
			func(_ *Manifest, tab *ChunkTable) { tab.Chunks[1].P = "backup/data/b.blob" },
			"instead of backup/data/a.blob",
		},
		"a chunk naming no blob": {
			func(_ *Manifest, tab *ChunkTable) { tab.Chunks[1].P = "" },
			"names no blob",
		},
		"two layers reading one blob": {
			func(m *Manifest, _ *ChunkTable) {
				m.Layers = []LayerInfo{
					{Index: 0, Digest: "sha256:a", ChunkFrom: 0, ChunkTo: 0, StoredBytes: 1000},
					{Index: 1, Digest: "sha256:b", ChunkFrom: 1, ChunkTo: 1, StoredBytes: 500},
				}
			},
			"both read backup/data/a.blob",
		},
		"a count the table does not have": {
			func(m *Manifest, _ *ChunkTable) { m.Chunking.Count = 3 },
			"the table holds 2",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, tab := limitsManifest(), limitsTable()
			tc.mutate(m, tab)
			err := ValidateChunkTable(m, tab)
			if !errors.Is(err, ErrBadSchema) {
				t.Fatalf("ValidateChunkTable = %v, want ErrBadSchema", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the refusal must say what disagrees, got %q", err)
			}
		})
	}
}

// TestTheChunkCapComesFromTheManifest states where the only bound without a
// counterpart in the files comes from, and that it is not invented when the
// manifest says nothing.
func TestTheChunkCapComesFromTheManifest(t *testing.T) {
	m := limitsManifest()
	if got, want := MaxStoredChunkBytes(m), int64(1024+16+4096); got != want {
		t.Fatalf("cap from the target size = %d, want %d", got, want)
	}
	m.Chunking.MaxChunkBytes = 4 << 20
	if got, want := MaxStoredChunkBytes(m), int64(4<<20+(4<<20)/64+4096); got != want {
		t.Fatalf("the declared maximum must win: %d, want %d", got, want)
	}
	m.Chunking.MaxChunkBytes, m.Chunking.TargetChunkBytes = 0, 0
	if got := MaxStoredChunkBytes(m); got != 0 {
		t.Fatalf("with nothing declared there is nothing to derive: %d", got)
	}
	// And with nothing to derive, the per-layer rules still hold.
	tab := limitsTable()
	tab.Chunks[0].Sb = 100 << 30
	if err := ValidateChunkTable(m, tab); !errors.Is(err, ErrBadSchema) {
		t.Fatalf("ValidateChunkTable = %v, want ErrBadSchema", err)
	}
}

// TestAnEmptyBackupIsNotAnError keeps the validator honest about the one
// legitimate table with no chunks in it.
func TestAnEmptyBackupIsNotAnError(t *testing.T) {
	m := limitsManifest()
	m.Chunking.Count = 0
	m.Layers = nil
	if err := ValidateChunkTable(m, &ChunkTable{SchemaVersion: SchemaVersion}); err != nil {
		t.Fatalf("a backup with no chunks must validate: %v", err)
	}
	m.Layers = []LayerInfo{{Index: 0, ChunkFrom: 0, ChunkTo: 0, StoredBytes: 10}}
	if err := ValidateChunkTable(m, &ChunkTable{SchemaVersion: SchemaVersion}); !errors.Is(err, ErrBadSchema) {
		t.Fatalf("a layer claiming chunks of an empty table = %v, want ErrBadSchema", err)
	}
}
