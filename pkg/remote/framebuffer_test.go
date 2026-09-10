package remote

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"
)

// TestFrameBufferPreservesTheStream checks the only thing a reordering or a
// lost buffer swap would break: the bytes, in order, exactly once.
func TestFrameBufferPreservesTheStream(t *testing.T) {
	for _, size := range []int{1, 7, 64, 4096} {
		payload := make([]byte, 40000)
		source := rand.New(rand.NewSource(3))
		if _, err := source.Read(payload); err != nil {
			t.Fatal(err)
		}
		var sink bytes.Buffer
		fb := NewFrameBuffer(&sink, size)
		// Writes of a size unrelated to the buffer, so partial fills, exact
		// fills and oversized writes all occur.
		for offset := 0; offset < len(payload); offset += 333 {
			end := min(offset+333, len(payload))
			n, err := fb.Write(payload[offset:end])
			if err != nil {
				t.Fatal(err)
			}
			if n != end-offset {
				t.Fatalf("short write: %d != %d", n, end-offset)
			}
		}
		if err := fb.Flush(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sink.Bytes(), payload) {
			t.Fatalf("buffer size %d: stream differs (%d bytes out of %d)", size, sink.Len(), len(payload))
		}
	}
}

// heldWriter blocks its first write until it is released, standing in for a
// send that is taking as long as a saturated link can take, and reports the
// highest number of writers it ever saw inside itself at once.
type heldWriter struct {
	release   chan struct{}
	held      chan struct{}
	first     atomic.Bool
	inside    atomic.Int32
	maxInside atomic.Int32
	sink      bytes.Buffer
}

func newHeldWriter() *heldWriter {
	return &heldWriter{release: make(chan struct{}), held: make(chan struct{})}
}

func (w *heldWriter) Write(p []byte) (int, error) {
	if n := w.inside.Add(1); n > w.maxInside.Load() {
		w.maxInside.Store(n)
	}
	if w.first.CompareAndSwap(false, true) {
		close(w.held)
		<-w.release
	}
	n, err := w.sink.Write(p)
	w.inside.Add(-1)
	return n, err
}

// TestFrameBufferOverlapsProductionWithSending is the point of the type: the
// producer must keep working while a buffer is on the wire. A plain
// bufio.Writer stops the archiver for the whole duration of every send, so a
// backup costs walk + send per frame instead of max(walk, send).
//
// Measuring that as elapsed time against the serial cost is what this
// replaces: on a slow runner the margin closes and the test fails for the
// speed of the machine (it did, on Windows, at 303ms against a 300ms bound).
// One send is held instead, for as long as it takes, and the question becomes
// exact: can the producer hand over another frame while that send is stuck?
func TestFrameBufferOverlapsProductionWithSending(t *testing.T) {
	const (
		bufSize = 1024
		frames  = 8
	)
	w := newHeldWriter()
	fb := NewFrameBuffer(w, bufSize)
	payload := bytes.Repeat([]byte{0xA5}, bufSize)

	// The first frame only fills a buffer: a send starts when the next write
	// finds no space left.
	if _, err := fb.Write(payload); err != nil {
		t.Fatal(err)
	}

	// The second write puts frame 1 on the wire, where it stays, and then has
	// to go somewhere: into the other buffer, which is the whole point. A
	// producer that waits for the send instead never returns from here.
	handedOver := make(chan error, 1)
	go func() {
		_, err := fb.Write(payload)
		handedOver <- err
	}()
	<-w.held
	select {
	case err := <-handedOver:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the producer stopped while a send was in flight: the buffers do not overlap")
	}
	close(w.release)

	for range frames - 2 {
		if _, err := fb.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := fb.Flush(); err != nil {
		t.Fatal(err)
	}

	// Ordering depends on there being at most one send in flight.
	if got := w.maxInside.Load(); got > 1 {
		t.Fatalf("%d concurrent writes: frames could be reordered", got)
	}
	if w.sink.Len() != frames*bufSize {
		t.Fatalf("sent %d bytes, want %d", w.sink.Len(), frames*bufSize)
	}
}

// TestFrameBufferErrorIsSticky makes sure a failed send is reported to the
// producer and not silently swallowed by the buffer behind it.
func TestFrameBufferErrorIsSticky(t *testing.T) {
	want := errors.New("wire is gone")
	fb := NewFrameBuffer(failingWriter{err: want}, 16)
	// The first buffer fills and starts a send that fails.
	if _, err := fb.Write(bytes.Repeat([]byte{1}, 16)); err != nil {
		t.Fatalf("first write already failed: %v", err)
	}
	// The failure surfaces at the next fill or at the flush, whichever
	// happens first, and it must keep surfacing afterwards.
	_, first := fb.Write(bytes.Repeat([]byte{1}, 16))
	flushErr := fb.Flush()
	if !errors.Is(first, want) && !errors.Is(flushErr, want) {
		t.Fatalf("the send failure never reached the producer: write=%v flush=%v", first, flushErr)
	}
	if _, err := fb.Write([]byte{1}); !errors.Is(err, want) {
		t.Fatalf("write after failure = %v, want the recorded cause", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write(p []byte) (int, error) { return 0, w.err }

var _ io.Writer = (*FrameBuffer)(nil)
