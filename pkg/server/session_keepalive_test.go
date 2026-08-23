package server

import (
	"sync"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
)

// fakeClock is read by the session goroutine and advanced by the test one.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// TestKeepalivesAloneDoNotHoldASession covers a peer that authenticates and
// then keeps the connection alive without ever making progress. The idle
// deadline cannot catch it — a keepalive every 30 seconds is not idle — so it
// would sit on one of the --max-sessions slots indefinitely.
func TestKeepalivesAloneDoNotHoldASession(t *testing.T) {
	clock := newFakeClock()
	client, done := startSession(t, SessionConfig{
		AllowNoAuth: true, TempDir: t.TempDir(), Now: clock.Now,
	}, newStreamSink())
	defer client.Close()

	writeHello(t, client, "", protocol.Version)
	// A background reader, as the real client has: net.Pipe carries no buffer,
	// so a server answering mid-write would deadlock a read-after-write test.
	peer := newStreamPeer(t, client)
	if ack := peer.next(t).GetHelloAck(); ack == nil {
		t.Fatal("no hello ack")
	}

	// Well inside the budget: accepted, and no answer is due for a keepalive.
	clock.Advance(maxKeepaliveOnly / 2)
	if err := protocol.WriteFrame(client, protocol.FrameKeepalive, nil); err != nil {
		t.Fatal(err)
	}

	// Past it: refused, with a network-kind error the client can act on.
	clock.Advance(maxKeepaliveOnly + time.Second)
	if err := protocol.WriteFrame(client, protocol.FrameKeepalive, nil); err != nil {
		t.Fatal(err)
	}
	failure := peer.next(t).GetError()
	if failure == nil || failure.Kind != ErrorNetwork {
		t.Fatalf("error = %v, want a network-kind refusal", failure)
	}
	if err := <-done; err == nil {
		t.Fatal("the session must end with an error")
	}
}

// TestProgressResetsTheKeepaliveBudget is the other half: a session doing real
// work is never cut off, however long it runs. Here the total elapsed time is
// more than twice the budget, but a real frame lands in the middle.
func TestProgressResetsTheKeepaliveBudget(t *testing.T) {
	clock := newFakeClock()
	client, done := startSession(t, SessionConfig{
		AllowNoAuth: true, TempDir: t.TempDir(), Now: clock.Now,
	}, newStreamSink())
	defer client.Close()

	writeHello(t, client, "", protocol.Version)
	peer := newStreamPeer(t, client)
	if ack := peer.next(t).GetHelloAck(); ack == nil {
		t.Fatal("no hello ack")
	}

	clock.Advance(maxKeepaliveOnly - time.Minute)
	if err := protocol.WriteFrame(client, protocol.FrameKeepalive, nil); err != nil {
		t.Fatal(err)
	}

	// Real progress: this is what resets the budget.
	writeClient(t, client, streamStartMessage("registry.test/me/repo:t", 1<<20))
	if ack := peer.next(t).GetStreamAck(); ack == nil || !ack.Ready {
		t.Fatalf("stream ack = %v", ack)
	}

	// Another near-budget gap. Cumulatively well past it, still accepted.
	clock.Advance(maxKeepaliveOnly - time.Minute)
	if err := protocol.WriteFrame(client, protocol.FrameKeepalive, nil); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{
		Msg: &protocol.ClientMessage_Cancel{Cancel: &protocol.Cancel{Reason: "done"}},
	})
	if err := <-done; err == nil {
		t.Fatal("cancel must end the session with an error")
	}
}
