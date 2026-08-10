package server

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
)

// streamPeer drives the client half of a streaming session: it reads server
// messages in the background, as the real client does, so progress messages
// never deadlock an unbuffered transport.
type streamPeer struct {
	conn net.Conn
	msgs chan *protocol.ServerMessage
	err  chan error
}

func newStreamPeer(t *testing.T, conn net.Conn) *streamPeer {
	t.Helper()
	peer := &streamPeer{conn: conn, msgs: make(chan *protocol.ServerMessage, 256), err: make(chan error, 1)}
	go func() {
		for {
			typ, payload, err := protocol.ReadFrame(conn, nil)
			if err != nil {
				peer.err <- err
				close(peer.msgs)
				return
			}
			if typ != protocol.FrameControl {
				peer.err <- errors.New("unexpected frame type")
				close(peer.msgs)
				return
			}
			msg, decErr := protocol.DecodeServerMessage(payload)
			if decErr != nil {
				peer.err <- decErr
				close(peer.msgs)
				return
			}
			peer.msgs <- msg
		}
	}()
	return peer
}

func (p *streamPeer) next(t *testing.T) *protocol.ServerMessage {
	t.Helper()
	select {
	case msg, ok := <-p.msgs:
		if !ok {
			t.Fatalf("server closed the session: %v", <-p.err)
		}
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for a server message")
		return nil
	}
}

func streamStartMessage(ref string, estimated uint64) *protocol.ClientMessage {
	return &protocol.ClientMessage{Msg: &protocol.ClientMessage_StreamStart{StreamStart: &protocol.StreamStart{
		Reference:      ref,
		ToolVersion:    "test",
		Archive:        &protocol.ArchiveConfig{Compression: "zstd"},
		Platforms:      []*protocol.Platform{{Os: "linux", Architecture: "amd64"}},
		EstimatedBytes: estimated,
		MaxLayerBytes:  8 << 20,
		Encryption:     &protocol.EncryptionConfig{},
	}}}
}

func TestSessionStreamPublishesAndReportsProgress(t *testing.T) {
	stream, _ := testArchive(t, 24<<20)
	sink := newStreamSink()
	client, done := startSession(t, SessionConfig{
		AllowNoAuth: true, TempDir: t.TempDir(), ProgressInterval: time.Nanosecond,
	}, sink)
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(60 * time.Second))

	writeHello(t, client, "", protocol.Version)
	peer := newStreamPeer(t, client)
	ack := peer.next(t).GetHelloAck()
	if ack == nil || !ack.Streaming || ack.ProtocolVersion != protocol.Version {
		t.Fatalf("hello ack = %v", ack)
	}
	writeClient(t, client, streamStartMessage("registry.test/me/repo:t", uint64(len(stream))))
	if streamAck := peer.next(t).GetStreamAck(); streamAck == nil || !streamAck.Ready {
		t.Fatalf("stream ack = %v", streamAck)
	}
	for offset := 0; offset < len(stream); offset += 1 << 20 {
		end := min(offset+(1<<20), len(stream))
		if err := protocol.WriteFrame(client, protocol.FrameData, stream[offset:end]); err != nil {
			t.Fatal(err)
		}
	}
	writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_StreamEnd{StreamEnd: &protocol.StreamEnd{
		RawBytes: uint64(len(stream)),
	}}})

	progress := 0
	var end *protocol.BackupEnd
	for end == nil {
		msg := peer.next(t)
		if p := msg.GetStreamProgress(); p != nil {
			progress++
			continue
		}
		if failure := msg.GetError(); failure != nil {
			t.Fatalf("server error: %v", failure)
		}
		end = msg.GetBackupEnd()
	}
	if end.Digest != "sha256:published" || end.Layers < 2 || end.Chunks == 0 || end.Files != 2 {
		t.Fatalf("backup end = %v", end)
	}
	if progress == 0 {
		t.Fatal("no StreamProgress was sent while receiving")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if sink.commits != 1 {
		t.Fatalf("commits = %d", sink.commits)
	}
}

func TestSessionStreamRejections(t *testing.T) {
	stream, _ := testArchive(t, 2<<20)
	tests := []struct {
		name string
		cfg  SessionConfig
		sink Sink
		run  func(*testing.T, net.Conn)
		kind uint32
	}{
		{
			name: "before hello", cfg: SessionConfig{AllowNoAuth: true}, sink: newStreamSink(), kind: ErrorUsage,
			run: func(t *testing.T, c net.Conn) {
				writeClient(t, c, streamStartMessage("registry.test/me/repo:t", 1))
			},
		},
		{
			name: "protocol v1 client", cfg: SessionConfig{AllowNoAuth: true}, sink: newStreamSink(), kind: ErrorUsage,
			run: func(t *testing.T, c net.Conn) {
				writeHello(t, c, "", protocol.MinVersion)
				if ack := readServer(t, c).GetHelloAck(); ack == nil || ack.Streaming {
					t.Fatalf("a v1 client must not be offered streaming: %v", ack)
				}
				writeClient(t, c, streamStartMessage("registry.test/me/repo:t", 1))
			},
		},
		{
			name: "sink cannot publish streams", cfg: SessionConfig{AllowNoAuth: true}, sink: newMemorySink(), kind: ErrorUsage,
			run: func(t *testing.T, c net.Conn) {
				if ack := hello(t, c, ""); ack.Streaming {
					t.Fatal("a sink without CommitStream must not advertise streaming")
				}
				writeClient(t, c, streamStartMessage("registry.test/me/repo:t", 1))
			},
		},
		{
			name: "acl", cfg: SessionConfig{AllowNoAuth: true, AllowedRepos: []string{"allowed/"}}, sink: newStreamSink(), kind: ErrorAuth,
			run: func(t *testing.T, c net.Conn) {
				hello(t, c, "")
				writeClient(t, c, streamStartMessage("denied/repo:t", 1))
			},
		},
		{
			name: "estimate quota", cfg: SessionConfig{AllowNoAuth: true, MaxBytes: 8}, sink: newStreamSink(), kind: ErrorUsage,
			run: func(t *testing.T, c net.Conn) {
				hello(t, c, "")
				writeClient(t, c, streamStartMessage("registry.test/me/repo:t", 4096))
			},
		},
		{
			name: "missing reference", cfg: SessionConfig{AllowNoAuth: true}, sink: newStreamSink(), kind: ErrorUsage,
			run: func(t *testing.T, c net.Conn) {
				hello(t, c, "")
				writeClient(t, c, streamStartMessage("", 1))
			},
		},
		{
			name: "data before stream start", cfg: SessionConfig{AllowNoAuth: true}, sink: newStreamSink(), kind: ErrorUsage,
			run: func(t *testing.T, c net.Conn) {
				hello(t, c, "")
				if err := protocol.WriteFrame(c, protocol.FrameData, []byte("x")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "declared size mismatch", cfg: SessionConfig{AllowNoAuth: true}, sink: newStreamSink(), kind: ErrorIntegrity,
			run: func(t *testing.T, c net.Conn) {
				hello(t, c, "")
				writeClient(t, c, streamStartMessage("registry.test/me/repo:t", uint64(len(stream))))
				if ack := readServer(t, c).GetStreamAck(); ack == nil || !ack.Ready {
					t.Fatalf("stream ack = %v", ack)
				}
				if err := protocol.WriteFrame(c, protocol.FrameData, stream); err != nil {
					t.Fatal(err)
				}
				writeClient(t, c, &protocol.ClientMessage{Msg: &protocol.ClientMessage_StreamEnd{StreamEnd: &protocol.StreamEnd{
					RawBytes: uint64(len(stream)) + 1,
				}}})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.TempDir = t.TempDir()
			cfg.ProgressInterval = time.Hour
			client, done := startSession(t, cfg, tc.sink)
			tc.run(t, client)
			if got := readServer(t, client).GetError(); got == nil || got.Kind != tc.kind {
				t.Fatalf("error = %v", got)
			}
			_ = client.Close()
			if err := <-done; err == nil {
				t.Fatal("session error missing")
			}
			assertNoSpool(t, cfg.TempDir)
		})
	}
}

func TestSessionStreamEnforcesQuotaWhileReceiving(t *testing.T) {
	stream, _ := testArchive(t, 4<<20)
	temp := t.TempDir()
	// The estimate fits the quota, the actual stream does not: a dishonest
	// estimate must not buy extra bytes.
	client, done := startSession(t, SessionConfig{
		AllowNoAuth: true, TempDir: temp, ProgressInterval: time.Hour, MaxBytes: 2 << 20,
	}, newStreamSink())
	hello(t, client, "")
	writeClient(t, client, streamStartMessage("registry.test/me/repo:t", 1<<20))
	if ack := readServer(t, client).GetStreamAck(); ack == nil || !ack.Ready {
		t.Fatalf("stream ack = %v", ack)
	}
	// The server stops reading as soon as the quota trips, so the writes run
	// in the background while the error is collected here.
	go func() {
		for offset := 0; offset < len(stream); offset += 1 << 20 {
			end := min(offset+(1<<20), len(stream))
			if err := protocol.WriteFrame(client, protocol.FrameData, stream[offset:end]); err != nil {
				return
			}
		}
	}()
	if got := readServer(t, client).GetError(); got == nil || got.Kind != ErrorUsage {
		t.Fatalf("error = %v", got)
	}
	_ = client.Close()
	if err := <-done; err == nil {
		t.Fatal("quota violation must end the session with an error")
	}
	assertNoSpool(t, temp)
}

func TestSessionStreamCancelAbortsPipeline(t *testing.T) {
	stream, _ := testArchive(t, 4<<20)
	temp := t.TempDir()
	client, done := startSession(t, SessionConfig{
		AllowNoAuth: true, TempDir: temp, ProgressInterval: time.Hour,
	}, newStreamSink())
	hello(t, client, "")
	writeClient(t, client, streamStartMessage("registry.test/me/repo:t", uint64(len(stream))))
	if ack := readServer(t, client).GetStreamAck(); ack == nil || !ack.Ready {
		t.Fatalf("stream ack = %v", ack)
	}
	if err := protocol.WriteFrame(client, protocol.FrameData, stream[:1<<20]); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_Cancel{Cancel: &protocol.Cancel{Reason: "user"}}})
	_ = client.Close()
	if err := <-done; err == nil {
		t.Fatal("cancel must end the session with an error")
	}
	assertNoSpool(t, temp)
}

func TestPipelineErrorKind(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind uint32
	}{
		{errStreamAborted, ErrorNetwork},
		{errors.New("registry upload failed"), ErrorNetwork},
		{errors.New("session quota exceeded"), ErrorUsage},
		{errors.New("something else"), ErrorGeneric},
	} {
		if got := pipelineErrorKind(tc.err); got != tc.kind {
			t.Fatalf("pipelineErrorKind(%v) = %d, want %d", tc.err, got, tc.kind)
		}
	}
}
