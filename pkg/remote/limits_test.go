package remote

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/server"
	"github.com/manprint/backimage/pkg/transport"
)

// floodingDialer is a peer that speaks just enough of the protocol to keep
// asking for credentials. It never delivers a BackupAck: the client stays in
// the loop that handles auxiliary messages, which is exactly where a token
// request is answered.
type floodingDialer struct {
	requests int
	actions  [][]string
	sent     atomic.Int32
}

func (d *floodingDialer) Name() string { return "pipe" }

func (d *floodingDialer) Dial(_ context.Context, _ string) (transport.Stream, error) {
	client, peer := net.Pipe()
	go d.serve(peer)
	return client, nil
}

func (d *floodingDialer) serve(peer net.Conn) {
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(20 * time.Second))
	// net.Pipe is unbuffered, so a peer that only writes deadlocks as soon as
	// the client answers with a token. Drain in parallel.
	go func() {
		for {
			if _, _, err := protocol.ReadFrame(peer, nil); err != nil {
				return
			}
		}
	}()
	ack := &protocol.ServerMessage{Msg: &protocol.ServerMessage_HelloAck{HelloAck: &protocol.HelloAck{
		ServerVersion: "hostile", ProtocolVersion: protocol.Version, Resumable: true,
	}}}
	if err := protocol.WriteServerMessage(peer, ack); err != nil {
		return
	}
	for i := 0; i < d.requests; i++ {
		actions := []string{"pull", "push"}
		if len(d.actions) > 0 {
			actions = d.actions[i%len(d.actions)]
		}
		msg := &protocol.ServerMessage{Msg: &protocol.ServerMessage_TokenRequest{TokenRequest: &protocol.TokenRequest{
			Repository: "me/repo", Actions: actions,
		}}}
		if err := protocol.WriteServerMessage(peer, msg); err != nil {
			return
		}
		d.sent.Add(1)
	}
	// The pipe closes when this returns. A client that answered the whole
	// flood finds the peer gone; one that applied the cap stopped long
	// before the last request was written.
}

// TestAPeerCannotKeepAskingForCredentials is A7.5 on the client side. A4.1
// bounded which scopes may be minted; it left how often unbounded. Each
// request costs a Provider.Get — a real round trip to the registry's
// authorisation endpoint, on the user's account — and restarts the refresh
// goroutine for that scope, and a server pays nothing to send another one.
func TestAPeerCannotKeepAskingForCredentials(t *testing.T) {
	dialer := &floodingDialer{requests: maxSessionTokenRequests * 4}
	provider := &countingProvider{}
	client, err := New(Config{
		Dialer: dialer, Address: "pipe", Provider: provider, Backoffs: []time.Duration{},
	})
	if err != nil {
		t.Fatal(err)
	}
	layer := testLayer(t, []byte("payload"))
	_, err = client.Upload(context.Background(), testBackup(layer))
	if err == nil {
		t.Fatal("the client answered an unbounded flood of credential requests")
	}
	if !strings.Contains(err.Error(), "refusing to keep minting") {
		t.Fatalf("error = %v, want the request cap to be the cause", err)
	}
	if got := int(provider.gets.Load()); got > maxSessionTokenRequests {
		t.Fatalf("the credential provider was called %d times, the session cap is %d", got, maxSessionTokenRequests)
	}
	// A refused scope is not a transient failure: retrying would hand the
	// same peer a fresh budget on every attempt.
	var remoteErr *Error
	if !errors.As(err, &remoteErr) || remoteErr.Kind != 3 {
		t.Fatalf("error = %v, want a non-transient authorisation refusal", err)
	}
}

// discardStream is a transport that accepts everything and returns nothing.
type discardStream struct{}

func (discardStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (discardStream) Write(p []byte) (int, error) { return len(p), nil }
func (discardStream) Close() error                { return nil }
func (discardStream) SetDeadline(time.Time) error { return nil }

// TestRenewalGoroutinesAreBoundedPerSession measures the resource the request
// cap alone does not bound: one refresher runs per scope for as long as the
// session lasts, and a peer that keeps asking must replace them, never add.
func TestRenewalGoroutinesAreBoundedPerSession(t *testing.T) {
	provider := &countingProvider{}
	client, err := New(Config{
		Dialer: &sessionDialer{}, Address: "pipe", Provider: provider, Backoffs: []time.Duration{},
	})
	if err != nil {
		t.Fatal(err)
	}
	guard, err := newScopeGuard("registry.invalid/me/repo:tag")
	if err != nil {
		t.Fatal(err)
	}
	conn := &connection{
		client: client, stream: discardStream{}, asyncErr: make(chan error, 1),
		cancel: func() {}, refresh: map[string]context.CancelFunc{}, scopes: guard,
	}
	t.Cleanup(conn.close)

	// The four distinct scopes the guard allows, asked for over and over.
	// String() joins the actions in the order they arrive, so these are four
	// different keys for the same repository.
	scopes := [][]string{{"pull"}, {"push"}, {"pull", "push"}, {"push", "pull"}}
	runtime.GC()
	before := runtime.NumGoroutine()
	asked := 0
	for i := 0; i < maxSessionTokenRequests; i++ {
		err := conn.provideToken(context.Background(), &protocol.TokenRequest{
			Repository: "me/repo", Actions: scopes[i%len(scopes)],
		})
		if err != nil {
			t.Fatalf("request %d was refused before the cap: %v", i, err)
		}
		asked++
		conn.refreshMu.Lock()
		live := len(conn.refresh)
		conn.refreshMu.Unlock()
		if live > maxSessionScopes {
			t.Fatalf("%d refreshers after %d requests, the cap is %d", live, asked, maxSessionScopes)
		}
	}
	if asked <= maxSessionScopes {
		t.Fatalf("the test asked %d times, it has to outrun the scope count", asked)
	}
	// Cancelled refreshers leave on their own schedule, so the count is
	// asserted as a steady state rather than instantly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.Gosched()
		if grew := runtime.NumGoroutine() - before; grew <= maxSessionScopes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after %d credential requests, %d scopes are allowed",
				runtime.NumGoroutine()-before, asked, maxSessionScopes)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The next request past the cap is refused, and the refusal is the count,
	// not the shape of the request.
	err = conn.provideToken(context.Background(), &protocol.TokenRequest{
		Repository: "me/repo", Actions: []string{"pull", "push"},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to keep minting") {
		t.Fatalf("request %d = %v, want the session cap to refuse it", asked+1, err)
	}
}

// TestTheClientChecksTheQuotaItWasToldAbout is the other half of A7.5: a
// limit the peer announces is verified here too, against the bytes actually
// sent, instead of being discovered when the server refuses a layer that has
// already crossed the wire.
func TestTheClientChecksTheQuotaItWasToldAbout(t *testing.T) {
	// The server announces a quota the estimate fits into and the real layer
	// does not: 4 KiB of payload against a 1 KiB ceiling.
	layer := testLayer(t, make([]byte, 4<<10))
	sink := newTestSink()
	dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true, MaxBytes: 1 << 10}, sink: sink}
	client, err := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{}})
	if err != nil {
		t.Fatal(err)
	}
	backup := testBackup(layer)
	// A truthful estimate would be refused by the server before anything is
	// sent; this one is the case the client had no answer for.
	backup.Start.EstimatedBytes = 1
	_, err = client.Upload(context.Background(), backup)
	if err == nil {
		t.Fatal("an upload past the announced quota was accepted")
	}
	if !strings.Contains(err.Error(), "remote quota exceeded") {
		t.Fatalf("error = %v, want the announced quota to be the cause", err)
	}
	if got := sink.opens.Load(); got != 0 {
		t.Fatalf("%d blob uploads were opened before the client stopped", got)
	}
}

// TestTheStreamStopsAtTheAnnouncedQuota is the same rule on the streaming
// path, where it matters more: the archive is produced while it is sent, so
// the estimate checked before the stream opened is only ever an estimate, and
// without this the whole archive travels before the server says no.
func TestTheStreamStopsAtTheAnnouncedQuota(t *testing.T) {
	sink := newStreamingSink()
	dialer := &sessionDialer{
		cfg:  server.SessionConfig{AllowNoAuth: true, MaxBytes: 4 << 10, TempDir: t.TempDir()},
		sink: sink,
	}
	client, err := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{}})
	if err != nil {
		t.Fatal(err)
	}
	var sent int
	start := streamStart(1)
	_, err = client.UploadStream(context.Background(), StreamBackup{
		Start: start,
		Source: func(_ context.Context, w io.Writer) error {
			n, err := w.Write(make([]byte, 64<<10))
			sent += n
			return err
		},
	})
	if err == nil {
		t.Fatal("the stream ran past the quota the server announced")
	}
	if !strings.Contains(err.Error(), "remote quota exceeded") {
		t.Fatalf("error = %v, want the announced quota to be the cause", err)
	}
	if sent != 0 {
		t.Fatalf("%d bytes crossed the wire before the client stopped", sent)
	}
}
