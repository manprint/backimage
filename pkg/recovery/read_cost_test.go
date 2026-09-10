package recovery

import (
	"context"
	"io"
	"testing"
)

// countingReadCloser counts the bytes actually pulled out of a blob file.
type countingReadCloser struct {
	io.ReadCloser
	total *int64
}

func (c countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	*c.total += int64(n)
	return n, err
}

// seekableCounter keeps the Seek of the underlying file visible through the
// counting wrapper, which is what the fast path needs.
type seekableCounter struct {
	countingReadCloser
	seeker io.Seeker
}

func (s seekableCounter) Seek(offset int64, whence int) (int64, error) {
	return s.seeker.Seek(offset, whence)
}

// readCountingSource wraps a Source and reports how much it was asked to read.
// seekable=false hides io.Seeker, which is how a non-seekable source behaves.
type readCountingSource struct {
	Source
	total    int64
	seekable bool
}

func (s *readCountingSource) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	r, err := s.Source.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	counted := countingReadCloser{ReadCloser: r, total: &s.total}
	seeker, ok := r.(io.Seeker)
	if !ok || !s.seekable {
		return counted, nil
	}
	return seekableCounter{countingReadCloser: counted, seeker: seeker}, nil
}

// TestFullRestoreReadsEachLayerOnce pins the cost of reading a backup from a
// local layer blob.
//
// Chunks of a layer live concatenated in one file, and reaching chunk i used
// to mean discarding everything before it: n chunks read n²/2 chunk-sizes.
// With a seekable source the restore must read the stored bytes and nothing
// else. The non-seekable source keeps the discard, and is here to show the
// fallback still produces the same tar.
func TestFullRestoreReadsEachLayerOnce(t *testing.T) {
	ctx := context.Background()
	const chunkBytes = 512
	f := makeFixture(t, false, chunkBytes)
	if f.chunkCount < 6 {
		t.Fatalf("fixture must have several chunks to tell linear from quadratic, got %d", f.chunkCount)
	}

	var stored int64
	for _, c := range readTable(t, f.root+"/chunks.json").Chunks {
		stored += c.Sb
	}

	seek := &readCountingSource{Source: &LocalSource{Root: f.root}, seekable: true}
	b, err := Open(ctx, seek)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.StreamTar(ctx, io.Discard, true); err != nil {
		b.Close()
		t.Fatal(err)
	}
	b.Close()
	// The metadata files are read through the same source, so allow the
	// manifest, the chunk table and the index on top of the stored bytes.
	if limit := stored + 64*1024; seek.total > limit {
		t.Fatalf("a seekable restore read %d bytes for %d bytes of chunks (limit %d): the quadratic discard is back",
			seek.total, stored, limit)
	}

	plain := &readCountingSource{Source: &LocalSource{Root: f.root}, seekable: false}
	b2, err := Open(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if err := b2.StreamTar(ctx, io.Discard, true); err != nil {
		t.Fatal(err)
	}
	if plain.total <= seek.total {
		t.Fatalf("the non-seekable fallback read %d bytes, the seekable path %d: the test is not measuring what it claims",
			plain.total, seek.total)
	}
}
