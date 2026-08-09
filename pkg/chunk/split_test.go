package chunk

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"testing"
	"testing/iotest"
)

func collect(t *testing.T, s Splitter) []Chunk {
	t.Helper()
	var out []Chunk
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		cp := *c
		cp.Data = append([]byte(nil), c.Data...)
		out = append(out, cp)
	}
}

func TestFixedBasics(t *testing.T) {
	data := make([]byte, 10<<20) // 10 MiB
	rand.New(rand.NewSource(7)).Read(data)
	s, err := NewFixed(bytes.NewReader(data), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "fixed" {
		t.Fatalf("Name = %q", s.Name())
	}
	chunks := collect(t, s)
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(chunks))
	}
	sizes := []int64{4 << 20, 4 << 20, 2 << 20}
	off := int64(0)
	for i, c := range chunks {
		if c.Index != i {
			t.Fatalf("chunk %d has index %d", i, c.Index)
		}
		if c.PlainBytes != sizes[i] {
			t.Fatalf("chunk %d: %d bytes, want %d", i, c.PlainBytes, sizes[i])
		}
		if int64(len(c.Data)) != sizes[i] {
			t.Fatalf("chunk %d: data len %d", i, len(c.Data))
		}
		if !bytes.Equal(c.Data, data[off:off+sizes[i]]) {
			t.Fatalf("chunk %d content mismatch", i)
		}
		wantSHA := sha256.Sum256(data[off : off+sizes[i]])
		if c.PlainSHA != wantSHA {
			t.Fatalf("chunk %d sha mismatch", i)
		}
		off += sizes[i]
	}
}

func TestFixedEmptyStream(t *testing.T) {
	s, err := NewFixed(bytes.NewReader(nil), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream: want EOF, got %v", err)
	}
}

func TestFixedExactMultiple(t *testing.T) {
	data := make([]byte, 8<<20)
	s, err := NewFixed(bytes.NewReader(data), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	chunks := collect(t, s)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.PlainBytes != 4<<20 {
			t.Fatalf("chunk %d: %d bytes, want 4 MiB", c.Index, c.PlainBytes)
		}
	}
}

func TestFixedBadSize(t *testing.T) {
	if _, err := NewFixed(bytes.NewReader(nil), 1<<20-1); !errors.Is(err, ErrBadSize) {
		t.Fatalf("too small: %v", err)
	}
	if _, err := NewFixed(bytes.NewReader(nil), 1<<30+1); !errors.Is(err, ErrBadSize) {
		t.Fatalf("too large: %v", err)
	}
}

func TestFixedBytewiseReader(t *testing.T) {
	data := make([]byte, 3<<20+123)
	rand.New(rand.NewSource(9)).Read(data)
	s, err := NewFixed(iotest.OneByteReader(bytes.NewReader(data)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, c := range collect(t, s) {
		joined = append(joined, c.Data...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("bytewise reader lost data")
	}
}

func TestFixedDeterministic(t *testing.T) {
	data := make([]byte, 5<<20)
	rand.New(rand.NewSource(11)).Read(data)
	a, _ := NewFixed(bytes.NewReader(data), 2<<20)
	b, _ := NewFixed(bytes.NewReader(data), 2<<20)
	ca, cb := collect(t, a), collect(t, b)
	if len(ca) != len(cb) {
		t.Fatalf("chunk counts differ")
	}
	for i := range ca {
		if ca[i].PlainSHA != cb[i].PlainSHA || !bytes.Equal(ca[i].Data, cb[i].Data) {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

func TestFixedAllocs(t *testing.T) {
	data := make([]byte, 100<<20)
	rand.New(rand.NewSource(13)).Read(data)
	s, _ := NewFixed(bytes.NewReader(data), 1<<20)
	allocs := testing.AllocsPerRun(100, func() {
		c, err := s.Next()
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		_ = c
	})
	if allocs > 1 {
		t.Fatalf("AllocsPerRun = %v, want <= 1", allocs)
	}
}

func TestFixedBufferReuse(t *testing.T) {
	data := make([]byte, 2<<20)
	rand.New(rand.NewSource(17)).Read(data)
	s, _ := NewFixed(bytes.NewReader(data), 1<<20)
	c1, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	first := c1.Data
	c2, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 && &first[0] != &c2.Data[0] {
		t.Fatal("buffer not reused between chunks")
	}
}
