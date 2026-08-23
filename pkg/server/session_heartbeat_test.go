package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
)

// stallingSink blocks the pipeline for as long as a slow registry would.
type stallingSink struct {
	*streamSink
	release chan struct{}
	stalled chan struct{}
	once    bool
}

func (s *stallingSink) OpenBlob(ctx context.Context, ref, digest string, size int64) (BlobWriter, error) {
	writer, err := s.streamSink.OpenBlob(ctx, ref, digest, size)
	if err != nil {
		return nil, err
	}
	if s.once {
		return writer, nil
	}
	s.once = true
	return &stallingBlobWriter{BlobWriter: writer, sink: s}, nil
}

type stallingBlobWriter struct {
	BlobWriter
	sink *stallingSink
}

func (w *stallingBlobWriter) Commit(ctx context.Context) error {
	close(w.sink.stalled)
	<-w.sink.release
	return w.BlobWriter.Commit(ctx)
}

// TestHeartbeatKeepsTheWireWarmWhileThePipelineStalls is what makes the client
// idle timeout survive a slow registry. Progress used to be emitted only when
// a data frame arrived, so the server fell silent exactly while it was busy:
// the client then saw nothing on a session that was in fact progressing, and
// dropped it on its own deadline.
func TestHeartbeatKeepsTheWireWarmWhileThePipelineStalls(t *testing.T) {
	stream, _ := testArchive(t, 24<<20)
	sink := &stallingSink{
		streamSink: newStreamSink(),
		release:    make(chan struct{}),
		stalled:    make(chan struct{}),
	}
	client, done := startLongSession(t, SessionConfig{
		AllowNoAuth: true, TempDir: t.TempDir(), ProgressInterval: 50 * time.Millisecond,
	}, sink)
	defer client.Close()

	writeHello(t, client, "", protocol.Version)
	peer := newStreamPeer(t, client)
	if ack := peer.next(t).GetHelloAck(); ack == nil || !ack.Streaming {
		t.Fatal("no streaming hello ack")
	}
	writeClient(t, client, streamStartMessage("registry.test/me/repo:t", uint64(len(stream))))
	if ack := peer.next(t).GetStreamAck(); ack == nil || !ack.Ready {
		t.Fatal("no stream ack")
	}

	// Feed the stream from a goroutine: it is expected to block once the
	// stalled upload backs the pipeline up.
	go func() {
		for offset := 0; offset < len(stream); offset += 1 << 20 {
			end := min(offset+(1<<20), len(stream))
			if err := protocol.WriteFrame(client, protocol.FrameData, stream[offset:end]); err != nil {
				return
			}
		}
		_ = protocol.WriteClientMessage(client, &protocol.ClientMessage{
			Msg: &protocol.ClientMessage_StreamEnd{StreamEnd: &protocol.StreamEnd{RawBytes: uint64(len(stream))}},
		})
	}()

	select {
	case <-sink.stalled:
	case <-time.After(20 * time.Second):
		t.Fatal("the pipeline never reached the stalled upload")
	}

	// While the upload is stuck the server must keep talking.
	beats := 0
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case msg, ok := <-peer.msgs:
			if !ok {
				t.Fatalf("server closed the session while stalled: %v", <-peer.err)
			}
			if msg.GetStreamProgress() != nil {
				beats++
			}
			if failure := msg.GetError(); failure != nil {
				t.Fatalf("server error while stalled: %v", failure)
			}
		case <-deadline:
			break collect
		}
	}
	if beats < 2 {
		t.Fatalf("received %d progress messages during a 2s stall: the server goes silent when busy", beats)
	}

	close(sink.release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the session did not finish after the upload was released")
	}
}

// startLongSession is startSession with a deadline that survives a deliberate
// stall: the shared helper caps a session at three seconds.
func startLongSession(t *testing.T, cfg SessionConfig, sink Sink) (net.Conn, <-chan error) {
	t.Helper()
	s, err := NewSession(cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	deadline := time.Now().Add(90 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), server) }()
	return client, done
}
