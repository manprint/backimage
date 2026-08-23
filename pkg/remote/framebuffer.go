package remote

import (
	"io"
	"sync"
)

// FrameBuffer overlaps producing bytes with sending them. The producer fills
// one buffer while the previous one is on the wire, instead of alternating
// with it: a plain bufio.Writer makes the archiver wait for every flush, so a
// backup runs at the speed of walk+send rather than at the speed of the slower
// of the two. Only one send is ever in flight, so the working set is two
// buffers and the ordering of the stream is unchanged.
type FrameBuffer struct {
	w io.Writer

	mu       sync.Mutex
	buf      []byte
	spare    []byte
	inflight chan error
	err      error
}

// NewFrameBuffer wraps w with two buffers of size bytes each.
func NewFrameBuffer(w io.Writer, size int) *FrameBuffer {
	if size <= 0 {
		size = StreamFrameSize
	}
	return &FrameBuffer{
		w:     w,
		buf:   make([]byte, 0, size),
		spare: make([]byte, 0, size),
	}
}

func (f *FrameBuffer) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	written := 0
	for len(p) > 0 {
		space := cap(f.buf) - len(f.buf)
		if space == 0 {
			if err := f.startSend(); err != nil {
				return written, err
			}
			space = cap(f.buf)
		}
		n := min(len(p), space)
		f.buf = append(f.buf, p[:n]...)
		p = p[n:]
		written += n
	}
	return written, nil
}

// Flush sends what is buffered and waits for the wire to drain. It must be
// called before the caller declares the stream complete.
func (f *FrameBuffer) Flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.startSend(); err != nil {
		return err
	}
	return f.wait()
}

// startSend hands the full buffer to a background write and swaps in the idle
// one. Callers hold f.mu.
func (f *FrameBuffer) startSend() error {
	if len(f.buf) == 0 {
		return f.err
	}
	// Wait for the previous send: the wire takes one buffer at a time, and
	// this is what keeps the frames in order.
	if err := f.wait(); err != nil {
		return err
	}
	payload := f.buf
	f.buf, f.spare = f.spare[:0], payload
	done := make(chan error, 1)
	f.inflight = done
	go func() {
		_, err := f.w.Write(payload)
		done <- err
	}()
	return nil
}

// wait collects the outcome of the send in flight, if any. Callers hold f.mu.
func (f *FrameBuffer) wait() error {
	if f.inflight == nil {
		return f.err
	}
	err := <-f.inflight
	f.inflight = nil
	// The buffer the send owned is idle again.
	f.spare = f.spare[:0]
	if err != nil && f.err == nil {
		f.err = err
	}
	return f.err
}
