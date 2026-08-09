package index

import (
	"testing"
)

// irregularTable: 10 chunks with deliberately varied plain sizes.
func irregularTable() *ChunkTable {
	sizes := []int64{100, 250, 50, 1000, 800, 3, 512, 4096, 2000, 100}
	ct := &ChunkTable{SchemaVersion: SchemaVersion}
	for i, s := range sizes {
		ct.Chunks = append(ct.Chunks, Chunk{
			I: i, P: "backup/data/" + pad3(i) + ".dat",
			Ps: "sha256:" + dig(i), Ss: "sha256:" + dig(i+200),
			Pb: s, Sb: s / 2,
		})
	}
	return ct
}

func TestChunkForOffset(t *testing.T) {
	l := NewLocator(irregularTable())
	cases := []struct {
		off     int64
		wantIdx int
		wantIn  int64
	}{
		{0, 0, 0},
		{99, 0, 99},
		{100, 1, 0},
		{349, 1, 249}, // chunk 1 is [100,350)
		{350, 2, 0},
		{399, 2, 49},
		{1402, 4, 2}, // chunks 3=50(350..400) 4=1000(400..1400) 5=3(1400..1403)
		{1405, 4, 5}, // chunk 4 is [1400,2200)
		{1406, 4, 6},
		{8804, 8, 8804 - 6811}, // chunk 8 is [6811,8811)
	}
	for _, tc := range cases {
		got, inner, err := l.ChunkForOffset(tc.off)
		if err != nil || got != tc.wantIdx || inner != tc.wantIn {
			t.Errorf("offset %d: got (%d,%d) err=%v, want (%d,%d)", tc.off, got, inner, err, tc.wantIdx, tc.wantIn)
		}
	}
	if _, _, err := l.ChunkForOffset(-1); err == nil {
		t.Error("negative offset must error")
	}
	if _, _, err := l.ChunkForOffset(8911); err == nil {
		t.Error("offset at end must error")
	}
	if l.TotalPlainBytes() != 8911 {
		t.Errorf("total %d, want 8911", l.TotalPlainBytes())
	}
}

func TestRange(t *testing.T) {
	l := NewLocator(irregularTable())
	cases := []struct {
		start, end int64
		from, to   int
	}{
		{0, 100, 0, 0},
		{99, 101, 0, 1},    // crosses the 0/1 boundary
		{100, 351, 1, 2},   // chunks 1..2
		{1400, 1406, 4, 4}, // inside chunk 4
		{0, 8911, 0, 9},    // the whole stream
		{1403, 7800, 4, 8}, // spans 4..8
	}
	for _, tc := range cases {
		from, to, err := l.Range(tc.start, tc.end)
		if err != nil || from != tc.from || to != tc.to {
			t.Errorf("range [%d,%d): got (%d,%d) err=%v, want (%d,%d)",
				tc.start, tc.end, from, to, err, tc.from, tc.to)
		}
	}
	if _, _, err := l.Range(-1, 10); err == nil {
		t.Error("negative start must error")
	}
	if _, _, err := l.Range(10, 10); err == nil {
		t.Error("empty range must error")
	}
	if _, _, err := l.Range(8900, 9000); err == nil {
		t.Error("range past end must error")
	}
}

func globIndex() *Index {
	return &Index{
		SchemaVersion: SchemaVersion,
		Entries: []FileEntry{
			{Path: "myfiles/a.txt", Type: TypeRegular, Mode: "0644", TarOffset: 0, Size: 10, SHA256: dig(1)},
			{Path: "myfiles/docs/readme.md", Type: TypeRegular, Mode: "0644", TarOffset: 1024, Size: 20, SHA256: dig(2)},
			{Path: "myfiles/docs/de/nested.txt", Type: TypeRegular, Mode: "0644", TarOffset: 2048, Size: 30, SHA256: dig(3)},
			{Path: "myfiles/report.tmp", Type: TypeRegular, Mode: "0644", TarOffset: 3072, Size: 40, SHA256: dig(4)},
			{Path: "unknown/x.tmp", Type: TypeRegular, Mode: "0644", TarOffset: 4096, Size: 50, SHA256: dig(5)},
		},
	}
}

func TestEntriesMatching(t *testing.T) {
	idx := globIndex()

	got, err := EntriesMatching(idx, []string{"myfiles/docs/**"}, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("docs/** -> %d entries (%v)", len(got), err)
	}
	if got[0].Path != "myfiles/docs/readme.md" || got[1].Path != "myfiles/docs/de/nested.txt" {
		t.Fatalf("unexpected docs entries: %+v", got)
	}

	got, err = EntriesMatching(idx, nil, []string{"**/*.tmp"})
	if err != nil || len(got) != 3 {
		t.Fatalf("exclude tmp -> %d (%v)", len(got), err)
	}
	for _, e := range got {
		if e.Path == "myfiles/report.tmp" || e.Path == "unknown/x.tmp" {
			t.Fatalf("%s must be excluded", e.Path)
		}
	}

	// directory pattern: matching an ancestor dir pulls descendants
	got, err = EntriesMatching(idx, []string{"myfiles/docs"}, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("dir pattern -> %d (%v)", len(got), err)
	}
	if got[0].Path != "myfiles/docs/readme.md" {
		t.Fatalf("unexpected: %+v", got)
	}

	// trailing "/" behaves like a directory match
	got, err = EntriesMatching(idx, []string{"myfiles/docs/"}, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("trailing slash: %d (%v)", len(got), err)
	}

	// no filters -> everything
	got, err = EntriesMatching(idx, nil, nil)
	if err != nil || len(got) != 5 {
		t.Fatalf("no filters -> %d (%v)", len(got), err)
	}

	// empty excludes are a no-op
	got, err = EntriesMatching(idx, []string{"myfiles/*"}, []string{})
	// ancestor rule: "myfiles/docs" matches the glob, its tree comes too
	if err != nil || len(got) != 4 {
		t.Fatalf("myfiles/* -> %d (%v): %+v", len(got), err, got)
	}

	// malformed pattern
	if _, err := EntriesMatching(idx, []string{"myfiles/["}, nil); err == nil {
		t.Fatal("bad glob must error")
	}
	if _, err := EntriesMatching(nil, nil, nil); err == nil {
		t.Fatal("nil index must error")
	}
}

// tiny-range test: extracting ONE file requires very few chunks.
func TestChunksForTinyFile(t *testing.T) {
	// 1000 files of 1024 bytes each; 1 MiB chunks -> one file is ~1.5 KiB
	idx := &Index{SchemaVersion: SchemaVersion}
	ct := &ChunkTable{SchemaVersion: SchemaVersion}
	var off int64
	for i := 0; i < 1000; i++ {
		idx.Entries = append(idx.Entries, FileEntry{
			Path: "f" + pad3(i) + ".txt", Type: TypeRegular, Mode: "0644",
			TarOffset: off, Size: 1024, SHA256: dig(i),
		})
		off += 512 + 1024
	}
	chunkSize := int64(1 << 20)
	nChunks := int((off + chunkSize - 1) / chunkSize)
	for i := 0; i < nChunks; i++ {
		start := int64(i) * chunkSize
		size := chunkSize
		if start+size > off {
			size = off - start
		}
		ct.Chunks = append(ct.Chunks, Chunk{
			I: i, P: "backup/data/" + pad4(i) + ".blob",
			Ps: "sha256:" + dig(i), Ss: "sha256:" + dig(i+500),
			Pb: size, Sb: size / 2,
		})
	}
	l := NewLocator(ct)

	// file 37 is fully inside one chunk: 1 chunk + no spill expected.
	one := []FileEntry{idx.Entries[37]}
	got, err := ChunksFor(l, one)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("single file needs %d chunks, want 1: %v", len(got), got)
	}

	// the whole backup needs everything
	all, err := ChunksFor(l, idx.Entries)
	if err != nil || len(all) != nChunks {
		t.Fatalf("full restore: %d chunks (%v), want %d", len(all), err, nChunks)
	}

	// worst single file (crossing a boundary): still < 3 chunks
	maxFiles := 0
	for _, e := range idx.Entries {
		c, err := ChunksFor(l, []FileEntry{e})
		if err != nil {
			t.Fatal(err)
		}
		if l := len(c); l > maxFiles {
			maxFiles = l
		}
	}
	if maxFiles >= 3 {
		t.Fatalf("worst single file needs %d chunks, want < 3", maxFiles)
	}

	// entry crossing into the void yields an error
	if _, err := ChunksFor(l, []FileEntry{{Path: "bogus", Type: TypeRegular, TarOffset: off - 100, Size: 500, Mode: "0644", SHA256: dig(9)}}); err == nil {
		t.Fatal("out-of-stream entry must error")
	}
}

func TestLocatorDegenerate(t *testing.T) {
	nilLoc := NewLocator(nil)
	if got := nilLoc.TotalPlainBytes(); got != 0 {
		t.Fatalf("total = %d", got)
	}
	if _, _, err := nilLoc.ChunkForOffset(0); err == nil {
		t.Fatal("nil table must error")
	}
	neg := NewLocator(&ChunkTable{SchemaVersion: 1, Chunks: []Chunk{{I: 0, P: "x", Pb: -5, Sb: 1}}})
	if _, _, err := neg.ChunkForOffset(0); err == nil {
		t.Fatal("negative size must error")
	}
	if _, _, err := neg.Range(0, 1); err == nil {
		t.Fatal("negative size range must error")
	}
	if _, _, err := neg.ChunkForOffset(3); err == nil {
		t.Fatal("out-of-range must error")
	}
}
