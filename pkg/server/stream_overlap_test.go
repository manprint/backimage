package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// heldSink blocks the first layer push until the client says it has handed
// over everything, or until the deadline.
//
// Sampling reception during a fixed sleep is what this replaces, and it does
// not measure the overlap: the pipeline is one layer deep on purpose, so a
// receiver fast enough to fill the next layer parks on the hand-off and looks
// blocked while it is only full. How often that happened depended on the speed
// of the runner, which is why the same test was green on Linux and red on
// macOS and Windows. Holding the push turns the question into one with a
// single answer: with one push stopped for as long as it takes, can the rest
// of the stream still reach the server?
type heldSink struct {
	*streamSink
	// sending is false once the client has written its last byte.
	sending *atomic.Bool

	pushes atomic.Int32
	// freed records whether the client got there before the deadline.
	freed atomic.Bool
}

func newHeldSink(sending *atomic.Bool) *heldSink {
	return &heldSink{streamSink: newStreamSink(), sending: sending}
}

// OpenBlob holds the first push at its first step, before the layer body is
// even read. Every later blob goes straight through.
func (s *heldSink) OpenBlob(ctx context.Context, ref, digest string, size int64) (BlobWriter, error) {
	if s.pushes.Add(1) == 1 {
		s.hold()
	}
	return s.streamSink.OpenBlob(ctx, ref, digest, size)
}

func (s *heldSink) hold() {
	// Generous: the deadline is only reached when the test is failing, and
	// the passing case leaves as soon as the client is done.
	const limit = 30 * time.Second

	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if !s.sending.Load() {
			s.freed.Store(true)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReceptionOverlapsTheRegistryPush is the bandwidth contract of the
// streaming server: a layer upload must not stop the wire. Performed on the
// receive path the push leaves the link idle for exactly as long as the
// slowest stage of the pipeline runs, which on a real registry is most of a
// backup.
//
// The payload is two layers, which is what the one-layer-deep pipeline can
// absorb: the first push is held, and the whole second layer still has to
// reach the server while it is. Serialise the two halves and the client stops
// at the first layer boundary instead.
func TestReceptionOverlapsTheRegistryPush(t *testing.T) {
	if testing.Short() {
		t.Skip("needs enough payload for two 16 MiB layers")
	}
	const layerBytes = 16 << 20
	stream, raw := testArchive(t, 28<<20)
	start, _ := streamStartFor(t, int(raw), false)
	start.MaxLayerBytes = layerBytes

	var sending atomic.Bool
	sending.Store(true)
	sink := newHeldSink(&sending)
	tempDir := t.TempDir()
	in, err := startIngest(context.Background(), ingestConfig{
		Start: start, SessionID: "overlap", Reference: start.Reference,
		TempDir: tempDir, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}

	for offset := 0; offset < len(stream); offset += 1 << 20 {
		end := min(offset+(1<<20), len(stream))
		if err := in.Write(stream[offset:end]); err != nil {
			t.Fatalf("write at %d: %v", offset, err)
		}
	}
	sending.Store(false)
	res, err := in.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if res.Layers < 2 {
		t.Fatalf("layers = %d, want two so a push can overlap the next one", res.Layers)
	}
	t.Logf("pushes=%d layers=%d", sink.pushes.Load(), res.Layers)
	if !sink.freed.Load() {
		t.Fatal("the client could not finish while one layer push was held: the push still blocks reception")
	}
	assertNoSpool(t, tempDir)
}

// TestConcurrentSessionsDoNotShareSpoolFiles covers two streaming sessions
// running against one --work-dir. A spool named after the layer index instead
// of a unique file lets the second session truncate the layer the first one is
// still uploading, and both publish silently corrupted data.
func TestConcurrentSessionsDoNotShareSpoolFiles(t *testing.T) {
	stream, raw := testArchive(t, 12<<20)
	tempDir := t.TempDir()

	const sessions = 3
	var wg sync.WaitGroup
	results := make([]StreamResult, sessions)
	errs := make([]error, sessions)
	for i := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start, _ := streamStartFor(t, int(raw), false)
			start.MaxLayerBytes = 4 << 20
			sink := newStreamSink()
			in, err := startIngest(context.Background(), ingestConfig{
				Start: start, SessionID: "session", Reference: start.Reference,
				TempDir: tempDir, Sink: sink,
			})
			if err != nil {
				errs[i] = err
				return
			}
			for offset := 0; offset < len(stream); offset += 1 << 20 {
				end := min(offset+(1<<20), len(stream))
				if writeErr := in.Write(stream[offset:end]); writeErr != nil {
					break
				}
			}
			results[i], errs[i] = in.Finish()
		}()
	}
	wg.Wait()

	for i := range sessions {
		if errs[i] != nil {
			t.Fatalf("session %d: %v", i, errs[i])
		}
	}
	// Same input, so every session must reach the same layer set. A spool
	// clobbered by a neighbour shows up here as a different digest or count.
	for i := 1; i < sessions; i++ {
		if results[i].Layers != results[0].Layers {
			t.Fatalf("session %d published %d layers, session 0 published %d",
				i, results[i].Layers, results[0].Layers)
		}
		if results[i].StoredBytes != results[0].StoredBytes {
			t.Fatalf("session %d stored %d bytes, session 0 stored %d",
				i, results[i].StoredBytes, results[0].StoredBytes)
		}
	}
	assertNoSpool(t, tempDir)
}

// TestSpoolNamesAreUnique is the direct statement of the same invariant.
func TestSpoolNamesAreUnique(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	for range 8 {
		spool, err := newSpool(dir)
		if err != nil {
			t.Fatal(err)
		}
		if seen[spool.path] {
			t.Fatalf("spool path %q reused: a layer still uploading would be truncated", spool.path)
		}
		seen[spool.path] = true
		if base := filepath.Base(spool.path); !strings.HasPrefix(base, "backimage-stream-") {
			t.Fatalf("unexpected spool name %q", base)
		}
		info, err := os.Stat(spool.path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 && runtime.GOOS != "windows" {
			t.Fatalf("spool mode = %o, want 600", perm)
		}
		t.Cleanup(spool.Remove)
	}
}
