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

// slowSink makes every registry upload take a fixed amount of time and
// records how many bytes the client managed to send while one was in flight.
// Sampling a flag between writes would be unreliable: the receiver unblocks
// exactly when an upload ends, so every sample would land on a quiet instant.
type slowSink struct {
	*streamSink
	delay time.Duration
	// received counts the bytes the client has handed over so far.
	received *atomic.Uint64

	uploads       atomic.Int32
	overlapBytes  atomic.Uint64
	blockedUpload atomic.Int32
}

func newSlowSink(delay time.Duration, received *atomic.Uint64) *slowSink {
	return &slowSink{streamSink: newStreamSink(), delay: delay, received: received}
}

func (s *slowSink) OpenBlob(ctx context.Context, ref, digest string, size int64) (BlobWriter, error) {
	writer, err := s.streamSink.OpenBlob(ctx, ref, digest, size)
	if err != nil {
		return nil, err
	}
	return &slowBlobWriter{BlobWriter: writer, sink: s}, nil
}

type slowBlobWriter struct {
	BlobWriter
	sink *slowSink
}

func (w *slowBlobWriter) Commit(ctx context.Context) error {
	w.sink.uploads.Add(1)
	before := w.sink.received.Load()
	time.Sleep(w.sink.delay)
	if moved := w.sink.received.Load() - before; moved > 0 {
		w.sink.overlapBytes.Add(moved)
	} else {
		w.sink.blockedUpload.Add(1)
	}
	return w.BlobWriter.Commit(ctx)
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
	const uploadDelay = 150 * time.Millisecond
	// chunk.DefaultLimits clamps a layer to 16 MiB, so the payload has to be
	// large enough to produce several of them.
	stream, raw := testArchive(t, 80<<20)
	start, _ := streamStartFor(t, int(raw), false)
	start.MaxLayerBytes = 16 << 20

	var received atomic.Uint64
	sink := newSlowSink(uploadDelay, &received)
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
	res, err := in.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if res.Layers < 3 {
		t.Fatalf("layers = %d, want several so an upload can overlap the next one", res.Layers)
	}
	t.Logf("uploads=%d overlapped-bytes=%d blocked-uploads=%d",
		sink.uploads.Load(), sink.overlapBytes.Load(), sink.blockedUpload.Load())
	if got := sink.overlapBytes.Load(); got == 0 {
		t.Fatalf("no byte was received during any of the %d uploads: the push still blocks reception",
			sink.uploads.Load())
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
