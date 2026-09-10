package server

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
)

// waitForSpool blocks until the receiving pipeline has opened its layer
// spool, so the test cancels a session that actually owns a temporary file
// rather than one that has not started writing yet.
func waitForSpool(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "backimage-stream-") {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the pipeline never opened a spool, so this test would measure nothing")
}

// TestACancelledSessionInsideTheRateLimiterLeavesNoSpool is B-A006.
//
// Every teardown of a session used to be written at the return that needed
// it, and the rate limiter is the one place that returns without going
// through fail(): it waits on the context, and a cancelled context is not a
// protocol error to report to a client that is no longer there. The session
// therefore returned while still owning a receiving pipeline, whose goroutine
// kept the spool of the layer it was assembling. Nothing removed it: the
// process was on its way out, which is exactly when --work-dir is a directory
// somebody else will reuse.
//
// The clock is frozen so the limiter is where the cancellation lands. With
// four kibibytes per second and two mebibytes already received, the session
// is parked in that sleep for the rest of the test, whatever the machine is
// doing.
func TestACancelledSessionInsideTheRateLimiterLeavesNoSpool(t *testing.T) {
	stream, _ := testArchive(t, 12<<20)
	temp := t.TempDir()
	frozen := time.Now()
	session, err := NewSession(SessionConfig{
		AllowNoAuth: true, TempDir: temp, ProgressInterval: time.Hour,
		RateLimit: 4 << 10, Now: func() time.Time { return frozen },
	}, newStreamSink())
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(60 * time.Second))
	_ = server.SetDeadline(time.Now().Add(60 * time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- session.Run(ctx, server) }()

	// The client half reads in the background, as the real one does: a
	// session that cannot deliver a message it decides to send would block in
	// the transport and never reach its own teardown, which would make this
	// test measure the test.
	writeHello(t, client, "", protocol.Version)
	peer := newStreamPeer(t, client)
	if ack := peer.next(t).GetHelloAck(); ack == nil || !ack.Streaming {
		t.Fatalf("hello ack = %v", ack)
	}
	writeClient(t, client, streamStartMessage("registry.test/me/repo:t", uint64(len(stream))))
	if ack := peer.next(t).GetStreamAck(); ack == nil || !ack.Ready {
		t.Fatalf("stream ack = %v", ack)
	}
	// One frame large enough to produce a chunk, then whatever the limiter
	// still lets through: the writer blocks on the pipe as soon as the server
	// stops reading, which is the point.
	go func() {
		for offset := 0; offset < len(stream); offset += 2 << 20 {
			end := min(offset+(2<<20), len(stream))
			if writeErr := protocol.WriteFrame(client, protocol.FrameData, stream[offset:end]); writeErr != nil {
				return
			}
		}
	}()
	waitForSpool(t, temp)

	cancel()
	select {
	case runErr := <-done:
		if runErr == nil {
			t.Fatal("a cancelled session must not report success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the session did not return after its context was cancelled")
	}
	assertNoSpool(t, temp)
}
