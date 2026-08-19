package backup

import (
	"strings"
	"testing"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/index"
)

func testDedupBase(t *testing.T, codec string, level int) *dedupBase {
	t.Helper()
	return &dedupBase{
		manifest: &index.Manifest{
			Archive: index.ArchiveInfo{Format: "tar", Compression: codec, CompressionLevel: level},
		},
	}
}

func mustCodec(t *testing.T, name string) compress.Codec {
	t.Helper()
	c, err := compress.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestResolveLevelInheritsFromDedupBase is the determinism guard for dedup.
//
// A chunk dedups only if it compresses to exactly the same bytes, so the level
// has to match the base backup. Without inheritance a codec default that moves
// between releases re-encodes every chunk and the hit rate drops to zero with
// nothing in the output to explain the upload.
func TestResolveLevelInheritsFromDedupBase(t *testing.T) {
	zstd := mustCodec(t, "zstd")
	_, _, zstdDefault := zstd.Levels()

	cases := []struct {
		name      string
		previous  *dedupBase
		dedup     bool
		requested int
		want      int
	}{
		{
			name:      "no base: codec default",
			previous:  nil,
			dedup:     true,
			requested: 0,
			want:      zstdDefault,
		},
		{
			name:      "base level adopted over the codec default",
			previous:  testDedupBase(t, "zstd", 4),
			dedup:     true,
			requested: 0,
			want:      4,
		},
		{
			name:      "explicit level always wins",
			previous:  testDedupBase(t, "zstd", 4),
			dedup:     true,
			requested: 1,
			want:      1,
		},
		{
			name:      "without --dedup nothing is inherited",
			previous:  testDedupBase(t, "zstd", 4),
			dedup:     false,
			requested: 0,
			want:      zstdDefault,
		},
		{
			name:      "base used another codec: default",
			previous:  testDedupBase(t, "gzip", 9),
			dedup:     true,
			requested: 0,
			want:      zstdDefault,
		},
		{
			name:      "base level out of range for this codec: default",
			previous:  testDedupBase(t, "zstd", 99),
			dedup:     true,
			requested: 0,
			want:      zstdDefault,
		},
		{
			name:      "base level zero: default",
			previous:  testDedupBase(t, "zstd", 0),
			dedup:     true,
			requested: 0,
			want:      zstdDefault,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveLevel(c.previous, zstd, c.dedup, c.requested); got != c.want {
				t.Fatalf("resolveLevel = %d, want %d", got, c.want)
			}
		})
	}
}

// TestResolveLevelIsStableAcrossRuns checks the property that actually matters:
// running the same command twice against a growing repository keeps producing the
// same level, so the blobs keep matching.
func TestResolveLevelIsStableAcrossRuns(t *testing.T) {
	zstd := mustCodec(t, "zstd")
	first := resolveLevel(nil, zstd, true, 0)
	// The first backup records what it used; the second one sees it as its base.
	second := resolveLevel(testDedupBase(t, "zstd", first), zstd, true, 0)
	third := resolveLevel(testDedupBase(t, "zstd", second), zstd, true, 0)
	if first != second || second != third {
		t.Fatalf("level drifted across runs: %d, %d, %d", first, second, third)
	}
}

func TestCompressionDedupWarning(t *testing.T) {
	zstd := mustCodec(t, "zstd")
	_, _, zstdDefault := zstd.Levels()

	if w := compressionDedupWarning(nil, "zstd", zstdDefault); w != "" {
		t.Fatalf("no base must not warn: %q", w)
	}
	if w := compressionDedupWarning(testDedupBase(t, "", 0), "zstd", zstdDefault); w != "" {
		t.Fatalf("a base without recorded compression must not warn: %q", w)
	}
	if w := compressionDedupWarning(testDedupBase(t, "zstd", zstdDefault), "zstd", zstdDefault); w != "" {
		t.Fatalf("matching codec and level must not warn: %q", w)
	}
	w := compressionDedupWarning(testDedupBase(t, "gzip", 6), "zstd", zstdDefault)
	if !strings.Contains(w, "gzip") || !strings.Contains(w, "zstd") {
		t.Fatalf("codec mismatch warning must name both codecs: %q", w)
	}
	w = compressionDedupWarning(testDedupBase(t, "zstd", 1), "zstd", 4)
	if !strings.Contains(w, "livello") || !strings.Contains(w, "4") || !strings.Contains(w, "1") {
		t.Fatalf("level mismatch warning must name both levels: %q", w)
	}
}
