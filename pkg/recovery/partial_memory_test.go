package recovery

import (
	"bytes"
	"context"
	"testing"
)

// widestWriter records the largest single Write it received, which is what a
// per-entry buffer shows up as: the whole entry arrives in one call.
type widestWriter struct {
	buf    bytes.Buffer
	widest int
}

func (w *widestWriter) Write(p []byte) (int, error) {
	if len(p) > w.widest {
		w.widest = len(p)
	}
	return w.buf.Write(p)
}

// TestPartialRecoveryDoesNotBufferAWholeEntry pins the memory bound of the
// partial path: it holds one chunk, not one entry.
//
// The fixture is built so an entry is larger than a chunk. The old readRange
// allocated end-start for the entry and wrote it in a single call, so its
// resident memory was the size of the largest file in the backup — a 50 GB
// file inside the archive meant 50 GB of process memory. The two-pass walk
// writes chunk-sized slices instead, and the widest write is therefore
// bounded by the chunk size.
func TestPartialRecoveryDoesNotBufferAWholeEntry(t *testing.T) {
	ctx := context.Background()
	const chunkBytes = 2048
	f := makeFixture(t, false, chunkBytes)
	b, err := OpenLocal(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	idx, err := b.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var biggest int64
	for _, e := range idx.Entries {
		if e.Size > biggest {
			biggest = e.Size
		}
	}
	if biggest <= chunkBytes {
		t.Fatalf("fixture must hold an entry larger than a chunk: biggest = %d, chunk = %d", biggest, chunkBytes)
	}

	var got widestWriter
	report, err := b.StreamTarPartial(ctx, idx, &got, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 0 || len(report.BadChunks) != 0 {
		t.Fatalf("intact backup reported losses: %+v", report)
	}
	if got.widest > chunkBytes {
		t.Fatalf("a single write carried %d bytes, more than the %d of one chunk", got.widest, chunkBytes)
	}
	if !bytes.Equal(got.buf.Bytes(), f.tarBytes) {
		t.Fatalf("partial recovery produced %d bytes, want the %d of the original tar", got.buf.Len(), len(f.tarBytes))
	}
}
