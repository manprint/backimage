package server

import (
	"context"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/registry"
)

// The scope the publishing sinks below ask their broker for.
var publishScope = registry.Scope{Repository: "me/repo", Actions: []string{"pull", "push"}}

// publishToken is the Token a client sends once its last message is out: the
// refresh the client sends on its own timer, which may land in that window.
func publishToken() *protocol.ClientMessage {
	return &protocol.ClientMessage{Msg: &protocol.ClientMessage_Token{Token: &protocol.Token{
		Value: "fresh", Repository: publishScope.Repository, Actions: publishScope.Actions,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	}}}
}

// tokenStreamSink publishes a streamed backup only with a registry token, as
// the registry sink does: the push of the image goes through its broker.
type tokenStreamSink struct {
	*streamSink
	broker *TokenBroker
}

func (s *tokenStreamSink) ProvideToken(tok *protocol.Token) { s.broker.ProvideToken(tok) }

func (s *tokenStreamSink) CommitStream(ctx context.Context, commit StreamCommit) (string, error) {
	if _, err := s.broker.Get(ctx, publishScope); err != nil {
		return "", err
	}
	return s.streamSink.CommitStream(ctx, commit)
}

// tokenBackupSink is the same for a v1 session.
type tokenBackupSink struct {
	*memorySink
	broker *TokenBroker
}

func (s *tokenBackupSink) ProvideToken(tok *protocol.Token) { s.broker.ProvideToken(tok) }

func (s *tokenBackupSink) CommitBackup(ctx context.Context, b Backup) (string, error) {
	if _, err := s.broker.Get(ctx, publishScope); err != nil {
		return "", err
	}
	return s.memorySink.CommitBackup(ctx, b)
}

// A streaming session publishes after StreamEnd, and the publication asks the
// broker for a token. The one it needs can be the refresh the client sent
// after StreamEnd: the session loop sat in the publication without reading,
// so that token stayed in the stream, the broker gave up and a backup whose
// data was all pushed failed.
func TestStreamPublishReadsTheTokenSentAfterStreamEnd(t *testing.T) {
	stream, _ := testArchive(t, 1<<20)
	sink := &tokenStreamSink{streamSink: newStreamSink(), broker: NewTokenBroker(2 * time.Second)}
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
	if err := protocol.WriteFrame(client, protocol.FrameData, stream); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{
		Msg: &protocol.ClientMessage_StreamEnd{StreamEnd: &protocol.StreamEnd{RawBytes: uint64(len(stream))}},
	})
	// The pipe is unbuffered: a server that does not read leaves this write
	// pending, which is the stall itself.
	sent := make(chan error, 1)
	go func() { sent <- protocol.WriteClientMessage(client, publishToken()) }()

	end := waitBackupEnd(t, peer)
	if end.Digest != "sha256:published" {
		t.Fatalf("digest = %q", end.Digest)
	}
	if err := <-sent; err != nil {
		t.Fatalf("token write: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("session: %v", err)
	}
}

// The v1 publication is the commit of the last layer and of the index, which
// runs while the client waits for its answer: the same token stall.
func TestLayerPublishReadsTheTokenSentAfterTheLastLayerEnd(t *testing.T) {
	data := []byte("layer contents")
	digest := digestOf(data)
	sink := &tokenBackupSink{memorySink: newMemorySink(), broker: NewTokenBroker(2 * time.Second)}
	client, done := startLongSession(t, SessionConfig{Version: "test", AllowNoAuth: true}, sink)
	defer client.Close()

	hello(t, client, "")
	writeClient(t, client, backupStart("registry.test/me/repo:t", 1, uint64(len(data))))
	if ack := readServer(t, client).GetBackupAck(); ack == nil || !ack.Ready {
		t.Fatalf("backup ack = %v", ack)
	}
	writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{
		Index: 0, Size: uint64(len(data)), Sha256: digest,
	}}})
	if ack := readServer(t, client).GetLayerAck(); ack == nil || ack.Skipped {
		t.Fatalf("layer ack = %v", ack)
	}
	if err := protocol.WriteFrame(client, protocol.FrameData, data); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerEnd{LayerEnd: &protocol.LayerEnd{Index: 0, Digest: digest}}})
	sent := make(chan error, 1)
	go func() { sent <- protocol.WriteClientMessage(client, publishToken()) }()

	peer := newStreamPeer(t, client)
	end := waitBackupEnd(t, peer)
	if end.Digest != "sha256:final" {
		t.Fatalf("digest = %q", end.Digest)
	}
	if err := <-sent; err != nil {
		t.Fatalf("token write: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("session: %v", err)
	}
}

// A keepalive during the publication is the client saying it is still there;
// a data frame is a client that did not wait, and the session refuses it
// instead of reading it as part of a backup that is already closed.
func TestStreamPublishRefusesDataAfterStreamEnd(t *testing.T) {
	stream, _ := testArchive(t, 1<<20)
	sink := &tokenStreamSink{streamSink: newStreamSink(), broker: NewTokenBroker(5 * time.Second)}
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
	if err := protocol.WriteFrame(client, protocol.FrameData, stream); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{
		Msg: &protocol.ClientMessage_StreamEnd{StreamEnd: &protocol.StreamEnd{RawBytes: uint64(len(stream))}},
	})
	go func() {
		_ = protocol.WriteFrame(client, protocol.FrameKeepalive, nil)
		_ = protocol.WriteFrame(client, protocol.FrameData, []byte("late"))
	}()
	for {
		select {
		case msg, ok := <-peer.msgs:
			if !ok {
				t.Fatalf("session closed without an error message: %v", <-peer.err)
			}
			if msg.GetBackupEnd() != nil {
				t.Fatal("a data frame after StreamEnd was accepted")
			}
			if e := msg.GetError(); e != nil {
				if e.Kind != ErrorUsage {
					t.Fatalf("error kind = %d (%s), want usage", e.Kind, e.Message)
				}
				if err := <-done; err == nil {
					t.Fatal("session returned nil after refusing the frame")
				}
				if sink.commits != 0 {
					t.Fatalf("published %d times after a protocol violation", sink.commits)
				}
				return
			}
		case <-time.After(20 * time.Second):
			t.Fatal("no answer to a data frame after StreamEnd")
		}
	}
}

func waitBackupEnd(t *testing.T, peer *streamPeer) *protocol.BackupEnd {
	t.Helper()
	for {
		select {
		case msg, ok := <-peer.msgs:
			if !ok {
				t.Fatalf("the session ended without a BackupEnd: %v", <-peer.err)
			}
			if e := msg.GetError(); e != nil {
				t.Fatalf("server error: %s", e.Message)
			}
			if end := msg.GetBackupEnd(); end != nil {
				return end
			}
		case <-time.After(20 * time.Second):
			t.Fatal("no BackupEnd")
		}
	}
}

// A client that cancels while the server publishes gets the publication
// stopped, not finished for nobody: the session used to notice the Cancel
// only once the publication had run its course.
func TestStreamPublishStopsOnCancel(t *testing.T) {
	stream, _ := testArchive(t, 1<<20)
	sink := &tokenStreamSink{streamSink: newStreamSink(), broker: NewTokenBroker(time.Minute)}
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
	if err := protocol.WriteFrame(client, protocol.FrameData, stream); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{
		Msg: &protocol.ClientMessage_StreamEnd{StreamEnd: &protocol.StreamEnd{RawBytes: uint64(len(stream))}},
	})
	go func() {
		_ = protocol.WriteClientMessage(client, &protocol.ClientMessage{
			Msg: &protocol.ClientMessage_Cancel{Cancel: &protocol.Cancel{Reason: "user interrupt"}},
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled session returned nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the publication ran on after the client cancelled")
	}
	if sink.commits != 0 {
		t.Fatalf("published %d times for a cancelled session", sink.commits)
	}
}

// commitStallSink holds the v1 index commit, as a slow registry would.
type commitStallSink struct {
	*memorySink
	stalled chan struct{}
	release chan struct{}
}

func (s *commitStallSink) CommitBackup(ctx context.Context, b Backup) (string, error) {
	close(s.stalled)
	<-s.release
	return s.memorySink.CommitBackup(ctx, b)
}

// Reading during the publication must not turn a long one into a timeout: a
// client waiting for BackupEnd is allowed to be silent, and a deadline that
// expires with nothing read only re-arms the wait.
func TestLayerPublishOutlivesASilentClient(t *testing.T) {
	const idle = 200 * time.Millisecond
	data := []byte("layer contents")
	digest := digestOf(data)
	sink := &commitStallSink{memorySink: newMemorySink(), stalled: make(chan struct{}), release: make(chan struct{})}
	client, done := startLongSession(t, SessionConfig{Version: "test", AllowNoAuth: true, IdleTimeout: idle}, sink)
	defer client.Close()

	hello(t, client, "")
	writeClient(t, client, backupStart("registry.test/me/repo:t", 1, uint64(len(data))))
	if ack := readServer(t, client).GetBackupAck(); ack == nil || !ack.Ready {
		t.Fatalf("backup ack = %v", ack)
	}
	writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{
		Index: 0, Size: uint64(len(data)), Sha256: digest,
	}}})
	if ack := readServer(t, client).GetLayerAck(); ack == nil || ack.Skipped {
		t.Fatalf("layer ack = %v", ack)
	}
	if err := protocol.WriteFrame(client, protocol.FrameData, data); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerEnd{LayerEnd: &protocol.LayerEnd{Index: 0, Digest: digest}}})
	peer := newStreamPeer(t, client)
	select {
	case <-sink.stalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the commit never started")
	}
	time.Sleep(5 * idle)
	close(sink.release)
	if end := waitBackupEnd(t, peer); end.Digest != "sha256:final" {
		t.Fatalf("digest = %q", end.Digest)
	}
	if err := <-done; err != nil {
		t.Fatalf("session: %v", err)
	}
}
