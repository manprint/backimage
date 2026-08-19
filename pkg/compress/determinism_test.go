package compress

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"testing"
)

// Deduplication rests entirely on one unstated assumption: the same plaintext
// chunk, compressed with the same codec and level, produces the same bytes every
// time and on every machine. If it does not, the stored blob digest changes, the
// registry sees a new layer and the hit rate silently collapses — with nothing
// in the output to explain the upload.
//
// The zstd encoder is the one that could plausibly break it, because it is
// configured with WithEncoderConcurrency(min(GOMAXPROCS,4)) for speed: the
// worker count varies with the machine and with the load. Measurement says
// klauspost/compress keeps the output independent of it, so the parallelism is
// kept. These tests exist to notice if a future version stops honouring that,
// since dedup would degrade quietly rather than fail loudly.

// determinismFixtures returns payloads that exercise different encoder paths:
// incompressible data takes the raw-block path, compressible data exercises
// match finding and block splitting. Sizes are a parameter because xz is slow
// enough that sweeping every codec over multi-megabyte inputs would dominate the
// package suite; only the zstd worker-count guard needs inputs large enough to
// span several blocks.
func determinismFixtures(sizes ...int) map[string][]byte {
	fixtures := map[string][]byte{}
	for _, size := range sizes {
		r := rand.New(rand.NewSource(11))
		noise := make([]byte, size)
		r.Read(noise)
		fixtures[fmt.Sprintf("noise-%d", size)] = noise

		var text bytes.Buffer
		words := []string{"alpha", "beta", "gamma", "INFO", "WARN", "user=admin",
			"2026-08-19T10:00:00Z", "path=/srv/data/file"}
		tr := rand.New(rand.NewSource(13))
		for text.Len() < size {
			fmt.Fprintf(&text, "%s %s %d\n", words[tr.Intn(len(words))], words[tr.Intn(len(words))], tr.Intn(1<<20))
		}
		fixtures[fmt.Sprintf("text-%d", size)] = text.Bytes()[:size]

		fixtures[fmt.Sprintf("zeros-%d", size)] = make([]byte, size)
	}
	return fixtures
}

func compressOnce(t *testing.T, c Codec, level int, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := c.NewWriter(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestCodecOutputIsReproducible checks that repeated compression of one payload
// yields byte-identical output for every codec and level. This is the property
// deduplication needs; a codec that embeds a timestamp or a random seed would
// fail here.
func TestCodecOutputIsReproducible(t *testing.T) {
	fixtures := determinismFixtures(1<<12, 1<<18)
	for _, name := range Names() {
		codec, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		minLevel, maxLevel, def := codec.Levels()
		levels := map[int]bool{def: true, minLevel: true, maxLevel: true}
		for level := range levels {
			for fixture, data := range fixtures {
				first := sha256.Sum256(compressOnce(t, codec, level, data))
				second := sha256.Sum256(compressOnce(t, codec, level, data))
				if first != second {
					t.Fatalf("%s level=%d %s: output is not reproducible", name, level, fixture)
				}
			}
		}
	}
}

// compressWithWorkers compresses data through the production zstd codec with the
// encoder concurrency pinned to procs.
func compressWithWorkers(t *testing.T, level, procs int, data []byte) []byte {
	t.Helper()
	original := zstdWorkers
	zstdWorkers = func() int { return procs }
	defer func() { zstdWorkers = original }()

	codec, err := Get("zstd")
	if err != nil {
		t.Fatal(err)
	}
	return compressOnce(t, codec, level, data)
}

// TestZstdOutputIndependentOfWorkerCount is the specific guard for the encoder
// concurrency in zstd.go. The worker count follows GOMAXPROCS, so if the output
// depended on it, the same chunk would compress differently on a 2-core and an
// 8-core machine and never dedup between them.
func TestZstdOutputIndependentOfWorkerCount(t *testing.T) {
	codec, err := Get("zstd")
	if err != nil {
		t.Fatal(err)
	}
	minLevel, maxLevel, _ := codec.Levels()
	fixtures := determinismFixtures(1<<12, 1<<20, 5<<20)

	for level := minLevel; level <= maxLevel; level++ {
		for fixture, data := range fixtures {
			var want [32]byte
			for i, procs := range []int{1, 2, 3, 4, 8} {
				got := sha256.Sum256(compressWithWorkers(t, level, procs, data))
				if i == 0 {
					want = got
					continue
				}
				if got != want {
					t.Fatalf("zstd level=%d %s: %d workers produce different bytes than 1 worker; "+
						"deduplication would silently break across machines. Pin "+
						"WithEncoderConcurrency(1) in zstd.go, or make the worker count part of "+
						"the manifest.", level, fixture, procs)
				}
			}
		}
	}
}
