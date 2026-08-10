package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
)

type memorySink struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	opened   int
	aborted  int
	commits  int
	backup   Backup
	known    []string
	knownErr error
}

func newMemorySink() *memorySink { return &memorySink{blobs: map[string][]byte{}} }

func (s *memorySink) KnownBlobs(context.Context, string) ([]string, error) {
	return append([]string(nil), s.known...), s.knownErr
}

func (s *memorySink) BlobExists(_ context.Context, _, digest string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[digest]
	return ok, nil
}

func (s *memorySink) OpenBlob(_ context.Context, _, digest string, _ int64) (BlobWriter, error) {
	s.mu.Lock()
	s.opened++
	s.mu.Unlock()
	return &memoryBlob{sink: s, digest: digest}, nil
}

func (s *memorySink) CommitBackup(_ context.Context, b Backup) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	s.backup = b
	return "sha256:final", nil
}

type memoryBlob struct {
	sink   *memorySink
	digest string
	buf    bytes.Buffer
}

func (b *memoryBlob) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *memoryBlob) Commit(context.Context) error {
	b.sink.mu.Lock()
	defer b.sink.mu.Unlock()
	b.sink.blobs[b.digest] = append([]byte(nil), b.buf.Bytes()...)
	return nil
}
func (b *memoryBlob) Abort(context.Context) error {
	b.sink.mu.Lock()
	b.sink.aborted++
	b.sink.mu.Unlock()
	return nil
}

func TestSessionHappyPathAndResumeSkip(t *testing.T) {
	data := []byte("layer contents")
	digest := digestOf(data)
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "upload", true: "skip"}[existing], func(t *testing.T) {
			sink := newMemorySink()
			if existing {
				sink.blobs[digest] = append([]byte(nil), data...)
				sink.known = []string{digest}
			}
			client, done := startSession(t, SessionConfig{Version: "test", AuthToken: []byte("shared")}, sink)
			defer client.Close()
			hello(t, client, "shared")
			writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_BackupStart{BackupStart: &protocol.BackupStart{
				Reference: "registry.test/me/repo:t", LayerCount: 1, EstimatedBytes: uint64(len(data)),
			}}})
			if ack := readServer(t, client).GetBackupAck(); ack == nil || !ack.Ready {
				t.Fatalf("backup ack = %v", ack)
			}
			writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{
				Index: 0, Size: uint64(len(data)), Sha256: digest,
			}}})
			ack := readServer(t, client).GetLayerAck()
			if ack == nil || ack.Skipped != existing {
				t.Fatalf("ack = %v", ack)
			}
			if !existing {
				if err := protocol.WriteFrame(client, protocol.FrameData, data[:4]); err != nil {
					t.Fatal(err)
				}
				if err := protocol.WriteFrame(client, protocol.FrameData, data[4:]); err != nil {
					t.Fatal(err)
				}
			}
			writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerEnd{LayerEnd: &protocol.LayerEnd{Index: 0, Digest: digest}}})
			progress := readServer(t, client).GetProgress()
			if progress == nil || progress.Skipped != existing {
				t.Fatalf("progress = %v", progress)
			}
			end := readServer(t, client).GetBackupEnd()
			if end == nil || end.Digest != "sha256:final" || end.BlobsSkipped != boolUint(existing) {
				t.Fatalf("backup end = %v", end)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if sink.commits != 1 || sink.opened != boolInt(!existing) {
				t.Fatalf("commits=%d opened=%d", sink.commits, sink.opened)
			}
		})
	}
}

func TestSessionAuthenticationVersionACLAndQuota(t *testing.T) {
	tests := []struct {
		name string
		cfg  SessionConfig
		run  func(*testing.T, net.Conn)
		kind uint32
	}{
		{
			name: "bad auth", cfg: SessionConfig{AuthToken: []byte("right")}, kind: ErrorAuth,
			run: func(t *testing.T, c net.Conn) {
				writeHello(t, c, "wrong", protocol.Version)
			},
		},
		{
			name: "bad version", cfg: SessionConfig{AuthToken: []byte("x")}, kind: ErrorUsage,
			run: func(t *testing.T, c net.Conn) {
				writeHello(t, c, "x", protocol.Version+1)
			},
		},
		{
			name: "acl", cfg: SessionConfig{AuthToken: []byte("x"), AllowedRepos: []string{"allowed/"}}, kind: ErrorAuth,
			run: func(t *testing.T, c net.Conn) {
				hello(t, c, "x")
				writeClient(t, c, backupStart("denied/repo:t", 1, 1))
			},
		},
		{
			name: "estimate quota", cfg: SessionConfig{AuthToken: []byte("x"), MaxBytes: 4}, kind: ErrorUsage,
			run: func(t *testing.T, c net.Conn) {
				hello(t, c, "x")
				writeClient(t, c, backupStart("repo:t", 1, 5))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := newMemorySink()
			client, done := startSession(t, tc.cfg, sink)
			tc.run(t, client)
			got := readServer(t, client).GetError()
			if got == nil || got.Kind != tc.kind {
				t.Fatalf("error = %v", got)
			}
			_ = client.Close()
			if err := <-done; err == nil {
				t.Fatal("session error missing")
			}
			if sink.opened != 0 {
				t.Fatalf("received bytes before policy rejection: opened=%d", sink.opened)
			}
		})
	}
}

func TestSessionRejectsInvalidTransitions(t *testing.T) {
	digest := digestOf([]byte("x"))
	cases := []struct {
		name string
		msg  *protocol.ClientMessage
		data bool
	}{
		{"backup before hello", backupStart("repo:t", 1, 1), false},
		{"layer before hello", &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{Index: 0, Size: 1, Sha256: digest}}}, false},
		{"end before hello", &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerEnd{LayerEnd: &protocol.LayerEnd{Index: 0, Digest: digest}}}, false},
		{"token before hello", &protocol.ClientMessage{Msg: &protocol.ClientMessage_Token{Token: &protocol.Token{Value: "secret"}}}, false},
		{"data before hello", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, done := startSession(t, SessionConfig{AllowNoAuth: true}, newMemorySink())
			if tc.data {
				if err := protocol.WriteFrame(client, protocol.FrameData, []byte("x")); err != nil {
					t.Fatal(err)
				}
			} else {
				writeClient(t, client, tc.msg)
			}
			if got := readServer(t, client).GetError(); got == nil || got.Kind != ErrorUsage {
				t.Fatalf("error = %v", got)
			}
			_ = client.Close()
			<-done
		})
	}
}

func TestSessionDigestAndSizeFailuresAbort(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		size uint64
		end  string
		kind uint32
	}{
		{"digest", []byte("bad"), 3, digestOf([]byte("good")), ErrorIntegrity},
		{"too much", []byte("xx"), 1, digestOf([]byte("x")), ErrorUsage},
		{"too little", []byte("x"), 2, digestOf([]byte("xx")), ErrorUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newMemorySink()
			client, done := startSession(t, SessionConfig{AllowNoAuth: true}, sink)
			hello(t, client, "")
			writeClient(t, client, backupStart("repo:t", 1, tc.size))
			if ack := readServer(t, client).GetBackupAck(); ack == nil || !ack.Ready {
				t.Fatalf("backup ack = %v", ack)
			}
			writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{Index: 0, Size: tc.size, Sha256: tc.end}}})
			_ = readServer(t, client).GetLayerAck()
			if err := protocol.WriteFrame(client, protocol.FrameData, tc.data); err != nil {
				t.Fatal(err)
			}
			if tc.name != "too much" {
				writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerEnd{LayerEnd: &protocol.LayerEnd{Index: 0, Digest: tc.end}}})
			}
			if got := readServer(t, client).GetError(); got == nil || got.Kind != tc.kind {
				t.Fatalf("error = %v", got)
			}
			_ = client.Close()
			<-done
			if sink.aborted != 1 {
				t.Fatalf("aborts = %d", sink.aborted)
			}
		})
	}
}

func TestSessionRateLimit(t *testing.T) {
	data := []byte("rate")
	digest := digestOf(data)
	client, done := startSession(t, SessionConfig{AllowNoAuth: true, RateLimit: 40}, newMemorySink())
	hello(t, client, "")
	writeClient(t, client, backupStart("repo:t", 1, uint64(len(data))))
	_ = readServer(t, client).GetBackupAck()
	writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{
		Index: 0, Size: uint64(len(data)), Sha256: digest,
	}}})
	_ = readServer(t, client).GetLayerAck()
	start := time.Now()
	if err := protocol.WriteFrame(client, protocol.FrameData, data); err != nil {
		t.Fatal(err)
	}
	writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerEnd{LayerEnd: &protocol.LayerEnd{Index: 0, Digest: digest}}})
	_ = readServer(t, client).GetProgress()
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("rate limit returned after %s", elapsed)
	}
	_ = readServer(t, client).GetBackupEnd()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNewSessionRequiresSinkAndAuthentication(t *testing.T) {
	if _, err := NewSession(SessionConfig{AllowNoAuth: true}, nil); err == nil {
		t.Fatal("nil sink accepted")
	}
	if _, err := NewSession(SessionConfig{}, newMemorySink()); err == nil {
		t.Fatal("unauthenticated server accepted")
	}
}

func TestSessionAdditionalProtocolErrors(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, net.Conn)
		kind uint32
	}{
		{"duplicate hello", func(t *testing.T, c net.Conn) { hello(t, c, ""); writeHello(t, c, "", protocol.Version) }, ErrorUsage},
		{"nonempty keepalive", func(t *testing.T, c net.Conn) {
			hello(t, c, "")
			if err := protocol.WriteFrame(c, protocol.FrameKeepalive, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}, ErrorUsage},
		{"empty control", func(t *testing.T, c net.Conn) { writeClient(t, c, new(protocol.ClientMessage)) }, ErrorUsage},
		{"invalid backup", func(t *testing.T, c net.Conn) {
			hello(t, c, "")
			writeClient(t, c, backupStart("", 0, 0))
		}, ErrorUsage},
		{"invalid layer", func(t *testing.T, c net.Conn) {
			hello(t, c, "")
			writeClient(t, c, backupStart("repo:t", 1, 1))
			_ = readServer(t, c).GetBackupAck()
			writeClient(t, c, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{Index: 1, Size: 1, Sha256: "bad"}}})
		}, ErrorUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, done := startSession(t, SessionConfig{AllowNoAuth: true}, newMemorySink())
			tc.run(t, client)
			if got := readServer(t, client).GetError(); got == nil || got.Kind != tc.kind {
				t.Fatalf("error = %v", got)
			}
			_ = client.Close()
			<-done
		})
	}
}

func TestSessionSinkFailureBranches(t *testing.T) {
	for _, mode := range []string{"known", "exists", "open", "short", "commit", "publish"} {
		t.Run(mode, func(t *testing.T) {
			sink := &faultSink{memorySink: newMemorySink(), mode: mode}
			client, done := startSession(t, SessionConfig{AllowNoAuth: true}, sink)
			writeHello(t, client, "", protocol.Version)
			if mode == "known" {
				if got := readServer(t, client).GetError(); got == nil || got.Kind != ErrorNetwork {
					t.Fatalf("error = %v", got)
				}
				_ = client.Close()
				<-done
				return
			}
			_ = readServer(t, client).GetHelloAck()
			writeClient(t, client, backupStart("repo:t", 1, 1))
			_ = readServer(t, client).GetBackupAck()
			digest := digestOf([]byte("x"))
			writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{Index: 0, Size: 1, Sha256: digest}}})
			if mode == "exists" || mode == "open" {
				if got := readServer(t, client).GetError(); got == nil || got.Kind != ErrorNetwork {
					t.Fatalf("error = %v", got)
				}
				_ = client.Close()
				<-done
				return
			}
			_ = readServer(t, client).GetLayerAck()
			if err := protocol.WriteFrame(client, protocol.FrameData, []byte("x")); err != nil {
				t.Fatal(err)
			}
			if mode == "short" {
				if got := readServer(t, client).GetError(); got == nil || got.Kind != ErrorNetwork {
					t.Fatalf("error = %v", got)
				}
				_ = client.Close()
				<-done
				return
			}
			writeClient(t, client, &protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerEnd{LayerEnd: &protocol.LayerEnd{Index: 0, Digest: digest}}})
			if mode == "publish" {
				if progress := readServer(t, client).GetProgress(); progress == nil {
					t.Fatal("progress missing before publish failure")
				}
			}
			if got := readServer(t, client).GetError(); got == nil || got.Kind != ErrorNetwork {
				t.Fatalf("error = %v", got)
			}
			_ = client.Close()
			<-done
		})
	}
}

type faultSink struct {
	*memorySink
	mode string
}

func (s *faultSink) KnownBlobs(ctx context.Context, id string) ([]string, error) {
	if s.mode == "known" {
		return nil, errors.New("known failure")
	}
	return s.memorySink.KnownBlobs(ctx, id)
}
func (s *faultSink) BlobExists(ctx context.Context, ref, digest string) (bool, error) {
	if s.mode == "exists" {
		return false, errors.New("exists failure")
	}
	return s.memorySink.BlobExists(ctx, ref, digest)
}
func (s *faultSink) OpenBlob(ctx context.Context, ref, digest string, size int64) (BlobWriter, error) {
	if s.mode == "open" {
		return nil, errors.New("open failure")
	}
	w, err := s.memorySink.OpenBlob(ctx, ref, digest, size)
	if err != nil {
		return nil, err
	}
	return &faultWriter{BlobWriter: w, mode: s.mode}, nil
}
func (s *faultSink) CommitBackup(ctx context.Context, b Backup) (string, error) {
	if s.mode == "publish" {
		return "", errors.New("publish failure")
	}
	return s.memorySink.CommitBackup(ctx, b)
}

type faultWriter struct {
	BlobWriter
	mode string
}

func (w *faultWriter) Write(p []byte) (int, error) {
	if w.mode == "short" {
		return 0, nil
	}
	return w.BlobWriter.Write(p)
}
func (w *faultWriter) Commit(ctx context.Context) error {
	if w.mode == "commit" {
		return errors.New("commit failure")
	}
	return w.BlobWriter.Commit(ctx)
}

func startSession(t *testing.T, cfg SessionConfig, sink Sink) (net.Conn, <-chan error) {
	t.Helper()
	s, err := NewSession(cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	deadline := time.Now().Add(3 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), server) }()
	return client, done
}

func hello(t *testing.T, c net.Conn, token string) *protocol.HelloAck {
	t.Helper()
	writeHello(t, c, token, protocol.Version)
	ack := readServer(t, c).GetHelloAck()
	if ack == nil {
		t.Fatal("HelloAck missing")
	}
	return ack
}

func writeHello(t *testing.T, c net.Conn, token string, version uint32) {
	t.Helper()
	writeClient(t, c, &protocol.ClientMessage{Msg: &protocol.ClientMessage_Hello{Hello: &protocol.Hello{
		ClientVersion: "test", ProtocolVersion: version, SessionId: "session", AuthToken: token,
	}}})
}

func backupStart(ref string, layers uint32, bytes uint64) *protocol.ClientMessage {
	return &protocol.ClientMessage{Msg: &protocol.ClientMessage_BackupStart{BackupStart: &protocol.BackupStart{
		Reference: ref, LayerCount: layers, EstimatedBytes: bytes,
	}}}
}

func writeClient(t *testing.T, c net.Conn, msg *protocol.ClientMessage) {
	t.Helper()
	if err := protocol.WriteClientMessage(c, msg); err != nil {
		t.Fatal(err)
	}
}

func readServer(t *testing.T, c net.Conn) *protocol.ServerMessage {
	t.Helper()
	typ, payload, err := protocol.ReadFrame(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if typ != protocol.FrameControl {
		t.Fatalf("frame type = %d", typ)
	}
	msg, err := protocol.DecodeServerMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func boolUint(v bool) uint32 { return uint32(boolInt(v)) }
