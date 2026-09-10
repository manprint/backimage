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

// heldSink blocks the first layer push and records how many bytes the client
// managed to hand over while it was held.
//
// Sampling reception inside a fixed sleep is what this replaces, and it does
// not measure the overlap: the pipeline is one layer deep on purpose, so a
// receiver fast enough to fill the next layer before the sample opens is
// parked on the hand-off for the whole window. It looks blocked while it is
// only full, and how often that happens depends on the speed of the runner —
// the same test passed on Linux and failed on macOS and Windows for that
// reason alone. Holding the push instead makes the question exact: with a
// whole layer of room free, does the wire keep moving?
type heldSink struct {
	*streamSink
	// received counts the bytes the client has handed over so far, sending
	// says whether it still has any left.
	received *atomic.Uint64
	sending  *atomic.Bool

	pushes atomic.Int32
	moved  atomic.Uint64
}

func newHeldSink(received *atomic.Uint64, sending *atomic.Bool) *heldSink {
	return &heldSink{streamSink: newStreamSink(), received: received, sending: sending}
}

// OpenBlob holds the first push at its very first step, before the layer body
// is even read, so the receiver still has its full layer budget available.
// Every later blob — the remaining layers, the config, the manifest — goes
// straight through.
func (s *heldSink) OpenBlob(ctx context.Context, ref, digest string, size int64) (BlobWriter, error) {
	if s.pushes.Add(1) == 1 {
		s.hold()
	}
	return s.streamSink.OpenBlob(ctx, ref, digest, size)
}

// hold keeps the push in place until reception stops moving on its own —
// the receiver has filled its one layer of room and parked on the hand-off —
// or until the client has nothing left to send. A push that stops the wire
// never moves a byte, so it releases on the deadline with nothing recorded.
func (s *heldSink) hold() {
	// Long enough that a runner scheduling the receiver late still counts as
	// movement, short enough that the passing case costs a fraction of a
	// second.
	const stall = 250 * time.Millisecond
	const limit = 10 * time.Second

	before := s.received.Load()
	last := before
	changed := time.Now()
	deadline := changed.Add(limit)
	for time.Now().Before(deadline) {
		now := s.received.Load()
		if now != last {
			last, changed = now, time.Now()
		}
		if !s.sending.Load() || (now > before && time.Since(changed) > stall) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.moved.Store(last - before)
}

// TestReceptionOverlapsTheRegistryPush is the bandwidth contract of the
// streaming server: a layer upload must not stop the wire. Performed on the
// receive path the push leaves the link idle for exactly as long as the
// slowest stage of the pipeline runs, which on a real registry is most of a
// backup.
func TestReceptionOverlapsTheRegistryPush(t *testing.T) {
	if testing.Short() {
		t.Skip("needs enough payload for several 16 MiB layers")
	}
	// chunk.DefaultLimits clamps a layer to 16 MiB, so the payload has to be
	// large enough to produce several of them.
	stream, raw := testArchive(t, 80<<20)
	start, _ := streamStartFor(t, int(raw), false)
	start.MaxLayerBytes = 16 << 20

	var received atomic.Uint64
	var sending atomic.Bool
	sending.Store(true)
	sink := newHeldSink(&received, &sending)
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
		received.Add(uint64(end - offset))
	}
	sending.Store(false)
	res, err := in.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if res.Layers < 3 {
		t.Fatalf("layers = %d, want several so an upload can overlap the next one", res.Layers)
	}
	t.Logf("pushes=%d received-while-the-first-push-was-held=%d", sink.pushes.Load(), sink.moved.Load())
	if sink.moved.Load() == 0 {
		t.Fatal("no byte was received while the first layer push was held: the push still blocks reception")
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
