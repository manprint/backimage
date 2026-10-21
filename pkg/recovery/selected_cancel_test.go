package recovery

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestStreamSelectedTarStopsOnACancelledContext: StreamTar and the partial
// stream checked the context between chunks, the selective stream did not,
// so an interrupted selective restore kept writing until its last span.
func TestStreamSelectedTarStopsOnACancelledContext(t *testing.T) {
	f := makeFixture(t, false, 8192)
	b, err := Open(context.Background(), &LocalSource{Root: f.root})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	idx, err := b.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	err = b.StreamSelectedTar(ctx, idx, idx.Entries, &out, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StreamSelectedTar on a cancelled context = %v, want context.Canceled", err)
	}
	if out.Len() != 0 {
		t.Fatalf("%d bytes written after the cancellation", out.Len())
	}
}
