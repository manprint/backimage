package server

import (
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
)

// TestALongPublishOutlivesTheIdleWindow is the regression for the deadline the
// session armed once per frame. The one armed to read StreamEnd was still the
// stream's deadline while the server published, so a publish longer than the
// idle timeout silenced the heartbeat and failed the final BackupEnd on a
// backup that had in fact been pushed. TCP hid it, rearming the deadline on
// every write; a stream with plain deadlines, as QUIC's are, did not.
func TestALongPublishOutlivesTheIdleWindow(t *testing.T) {
	const idle = 300 * time.Millisecond
	stream, _ := testArchive(t, 1<<20) // one layer, pushed after StreamEnd
	sink := &stallingSink{
		streamSink: newStreamSink(),
		release:    make(chan struct{}),
		stalled:    make(chan struct{}),
	}
	client, done := startLongSession(t, SessionConfig{
		AllowNoAuth: true, TempDir: t.TempDir(), IdleTimeout: idle, ProgressInterval: 50 * time.Millisecond,
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
	if err := protocol.WriteFrame(client, protocol.FrameData, stream); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{
		Msg: &protocol.ClientMessage_StreamEnd{StreamEnd: &protocol.StreamEnd{RawBytes: uint64(len(stream))}},
	})
	select {
	case <-sink.stalled:
	case <-time.After(20 * time.Second):
		t.Fatal("the publish never reached the stalled upload")
	}

	// Past the idle window the server must still be talking.
	late := 0
	quiet := time.After(2 * idle)
	stop := time.After(5 * idle)
collect:
	for {
		select {
		case msg, ok := <-peer.msgs:
			if !ok {
				t.Fatalf("server closed the session during the publish: %v", <-peer.err)
			}
			if msg.GetStreamProgress() != nil && quiet == nil {
				late++
			}
		case <-quiet:
			quiet = nil
		case <-stop:
			break collect
		}
	}
	if late == 0 {
		t.Fatal("no heartbeat after the idle window: the publish went silent")
	}

	close(sink.release)
	var end *protocol.BackupEnd
	for end == nil {
		select {
		case msg, ok := <-peer.msgs:
			if !ok {
				t.Fatalf("the session ended without a BackupEnd: %v", <-peer.err)
			}
			end = msg.GetBackupEnd()
		case <-time.After(20 * time.Second):
			t.Fatal("no BackupEnd after the publish was released")
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("session: %v", err)
	}
}
