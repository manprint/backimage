package chunk

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
)

func cdcChunks(t *testing.T, r io.Reader, p CDCParams) []*Chunk {
	t.Helper()
	s := NewCDC(r, p)
	var out []*Chunk
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, &Chunk{Index: c.Index, PlainBytes: c.PlainBytes, PlainSHA: c.PlainSHA})
	}
}

func deterministicBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	if _, err := rand.New(rand.NewSource(seed)).Read(b); err != nil {
		panic(err)
	}
	return b
}

func TestCDCDeterministicAndShiftResilient(t *testing.T) {
	// This is deliberately 100 MiB: a small test would not demonstrate that
	// Rabin boundaries re-synchronise after an insertion.
	data := deterministicBytes(100<<20, 42)
	p := DefaultCDCParams()
	a := cdcChunks(t, bytes.NewReader(data), p)
	b := cdcChunks(t, bytes.NewReader(data), p)
	if len(a) != len(b) {
		t.Fatalf("determinism: %d chunks then %d", len(a), len(b))
	}
	for i := range a {
		if a[i].PlainSHA != b[i].PlainSHA || a[i].PlainBytes != b[i].PlainBytes {
			t.Fatalf("determinism mismatch at chunk %d", i)
		}
	}

	shifted := append([]byte{'x'}, data...)
	c := cdcChunks(t, bytes.NewReader(shifted), p)
	seen := make(map[[32]byte]bool, len(c))
	for _, ck := range c {
		seen[ck.PlainSHA] = true
	}
	shared := 0
	for _, ck := range a {
		if seen[ck.PlainSHA] {
			shared++
		}
	}
	if ratio := float64(shared) / float64(len(a)); ratio < .90 {
		t.Fatalf("one-byte shift retained %.1f%% chunks, want >=90%% (%d/%d)", ratio*100, shared, len(a))
	}
}

func TestCDCRespectsBoundsAndAverage(t *testing.T) {
	p := DefaultCDCParams()
	for name, data := range map[string][]byte{
		"zeros":      make([]byte, 64<<20),
		"repetitive": bytes.Repeat([]byte("abc123"), (64<<20)/6),
		"random":     deterministicBytes(256<<20, 7),
	} {
		t.Run(name, func(t *testing.T) {
			chunks := cdcChunks(t, bytes.NewReader(data), p)
			if len(chunks) == 0 {
				t.Fatal("no chunks")
			}
			var total int64
			for i, c := range chunks {
				total += c.PlainBytes
				if i < len(chunks)-1 && (c.PlainBytes < p.Min || c.PlainBytes > p.Max) {
					t.Fatalf("chunk %d=%d outside [%d,%d]", i, c.PlainBytes, p.Min, p.Max)
				}
			}
			if total != int64(len(data)) {
				t.Fatalf("chunk bytes=%d want=%d", total, len(data))
			}
		})
	}

	// The random corpus is intentionally large enough to make the mean stable.
	chunks := cdcChunks(t, bytes.NewReader(deterministicBytes(512<<20, 11)), p)
	var total int64
	for _, c := range chunks {
		total += c.PlainBytes
	}
	avg := total / int64(len(chunks))
	if avg < p.Avg*3/4 || avg > p.Avg*5/4 {
		t.Fatalf("average=%d, want within 25%% of %d", avg, p.Avg)
	}
}

func TestCDCParameterValidation(t *testing.T) {
	if _, err := NormalizeCDCParams(CDCParams{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []CDCParams{
		{Min: MinChunkSize - 1},
		{Min: 8 << 20, Avg: 4 << 20, Max: 16 << 20},
		{Min: 1 << 20, Avg: 4 << 20, Max: MaxChunkSize + 1},
		{Polynomial: 1},
	} {
		if _, err := NormalizeCDCParams(p); !errors.Is(err, ErrBadCDCParams) {
			t.Fatalf("NormalizeCDCParams(%+v) = %v", p, err)
		}
	}
	if _, err := NewCDC(bytes.NewReader(nil), CDCParams{Polynomial: 1}).Next(); !errors.Is(err, ErrBadCDCParams) {
		t.Fatalf("invalid splitter error = %v", err)
	}
}
