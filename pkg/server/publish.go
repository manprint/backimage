package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/manprint/backimage/pkg/protocol"
)

// publishWhileReading runs the last step of a session — the publication —
// while it keeps reading the client.
//
// Publishing makes registry requests, and the token they need may be the
// refresh the client sent after its last message. The session loop used to
// sit in the publication without reading, so that token stayed in the stream
// behind it, the broker gave up waiting, and a backup whose data had all been
// pushed failed. The client is waiting for BackupEnd here: all it may send is
// keepalives, tokens and a cancellation. Anything else stops the publication
// and ends the session as a protocol error.
//
// The reader outlives the publication by design: the session ends with it,
// and closing the stream releases the read still pending then.
func (s *Session) publishWhileReading(ctx context.Context, stream *sessionStream, rs *runState, publish func() error) (publishErr, sessionErr error) {
	// published is written before done closes, and read only after.
	var published error
	done := make(chan struct{})
	go func() {
		defer close(done)
		published = publish()
	}()
	frames := make(chan frameRead)
	stop := make(chan struct{})
	defer close(stop)
	go readWhilePublishing(stream, frames, stop)
	for {
		select {
		case <-done:
			return published, nil
		case f := <-frames:
			if f.err == nil && f.typ == protocol.FrameKeepalive && len(f.payload) == 0 {
				continue
			}
			if f.err == nil && f.typ == protocol.FrameControl {
				if msg, err := protocol.DecodeClientMessage(f.payload); err == nil && msg.GetToken() != nil {
					if err := s.control(ctx, stream, rs, msg); err != nil {
						rs.cancel()
						<-done
						return nil, err
					}
					continue
				}
			}
			// Stop the publication before answering: nothing is pushed for a
			// client that has gone or broken the protocol.
			rs.cancel()
			<-done
			return nil, s.endPublish(context.WithoutCancel(ctx), stream, rs, f)
		}
	}
}

// endPublish ends a session on a frame that may not arrive during the
// publication. ctx is not the cancelled session context: releasing the
// registry upload still needs one that works.
func (s *Session) endPublish(ctx context.Context, stream *sessionStream, rs *runState, f frameRead) error {
	switch {
	case f.err != nil:
		return f.err
	case f.typ == protocol.FrameKeepalive:
		return s.fail(ctx, stream, rs, ErrorUsage, "keepalive frame must be empty", "")
	case f.typ == protocol.FrameData:
		return s.fail(ctx, stream, rs, ErrorUsage, "unexpected Data frame while publishing", "wait for BackupEnd")
	case f.typ == protocol.FrameControl:
		msg, err := protocol.DecodeClientMessage(f.payload)
		if err != nil {
			return s.fail(ctx, stream, rs, ErrorUsage, err.Error(), "")
		}
		switch msg.Msg.(type) {
		case *protocol.ClientMessage_Cancel, *protocol.ClientMessage_Error:
			return s.control(ctx, stream, rs, msg)
		}
		return s.fail(ctx, stream, rs, ErrorUsage, fmt.Sprintf("unexpected %T while publishing", msg.GetMsg()), "wait for BackupEnd")
	default:
		return s.fail(ctx, stream, rs, ErrorUsage, "unknown frame type", "")
	}
}

// frameRead is one frame, or the error that ended the reading.
type frameRead struct {
	typ     protocol.FrameType
	payload []byte
	err     error
}

// readWhilePublishing reads the client for publishWhileReading. A client
// waiting for BackupEnd may well say nothing for longer than the idle window —
// a long publication is not its silence, and the heartbeat keeps the wire
// warm — so a deadline that expires before any byte of a frame arrived only
// re-arms the wait; one that cuts a frame in half is a broken stream. Each
// frame gets its own buffer, since the consumer may still hold the previous
// one.
func readWhilePublishing(stream *sessionStream, out chan<- frameRead, stop <-chan struct{}) {
	counted := &countingStream{r: stream}
	for {
		var f frameRead
		if err := stream.extend(); err != nil {
			f.err = fmt.Errorf("set session deadline: %w", err)
		} else {
			counted.n = 0
			f.typ, f.payload, f.err = protocol.ReadFrame(counted, nil)
			if f.err != nil && counted.n == 0 && isTimeout(f.err) {
				select {
				case <-stop:
					return
				default:
					continue
				}
			}
		}
		select {
		case out <- f:
		case <-stop:
			return
		}
		if f.err != nil {
			return
		}
	}
}

// countingStream counts the bytes read since the last reset.
type countingStream struct {
	r io.Reader
	n int64
}

func (c *countingStream) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// isTimeout reports the expiry of the deadline the session armed, which TCP,
// TLS and QUIC all surface as os.ErrDeadlineExceeded. It is not
// net.Error.Timeout: that is also true of ETIMEDOUT from a peer the kernel has
// given up on, and waiting that out would keep a dead session alive.
func isTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}
