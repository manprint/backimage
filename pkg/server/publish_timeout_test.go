package server

import (
	"net"
	"syscall"
	"testing"
	"time"
)

// timedOutStream fails every read with a timeout that is not the session's
// own deadline: the kernel giving up on a dead peer.
type timedOutStream struct{}

func (s *timedOutStream) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ETIMEDOUT}
}
func (s *timedOutStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *timedOutStream) Close() error                { return nil }
func (s *timedOutStream) SetDeadline(time.Time) error { return nil }

// Only the deadline the session armed may be waited out while publishing. Any
// error whose Timeout() was true counted, ETIMEDOUT from a dead TCP peer
// included, and the reader re-armed on it for as long as the publication ran
// instead of reporting a connection that was gone.
func TestPublishReaderReportsATimeoutThatIsNotTheDeadline(t *testing.T) {
	raw := &timedOutStream{}
	stream := &sessionStream{Stream: raw, idle: time.Minute, now: time.Now}
	out := make(chan frameRead, 1)
	stop := make(chan struct{})
	defer close(stop)
	go readWhilePublishing(stream, out, stop)
	select {
	case f := <-out:
		if f.err == nil {
			t.Fatalf("frame = %+v, want the read error", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reader kept retrying a connection the kernel had timed out")
	}
}
