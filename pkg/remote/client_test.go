package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/ociimg"
	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/registry"
	"github.com/manprint/backimage/pkg/server"
	"github.com/manprint/backimage/pkg/transport"
)

func TestClientUploadAndResumeSkip(t *testing.T) {
	layer := testLayer(t, []byte("remote data"))
	digest, _ := layer.Digest()
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "upload", true: "skip"}[existing], func(t *testing.T) {
			sink := newTestSink()
			if existing {
				r, _ := layer.Compressed()
				sink.blobs[digest.String()], _ = io.ReadAll(r)
				_ = r.Close()
			}
			dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true}, sink: sink}
			client, err := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Upload(context.Background(), testBackup(layer))
			if err != nil {
				t.Fatal(err)
			}
			if result.Digest != "sha256:done" || result.BlobsSkipped != bool32(existing) || result.Attempts != 1 {
				t.Fatalf("result = %#v", result)
			}
			if sink.commits.Load() != 1 || sink.opens.Load() != int32(boolIntRemote(!existing)) {
				t.Fatalf("commits=%d opens=%d", sink.commits.Load(), sink.opens.Load())
			}
		})
	}
}

func TestClientUploadOverQUIC(t *testing.T) {
	cert, pin, err := transport.SelfSignedCertificate(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := transport.NewListener("quic", "127.0.0.1:0", transport.Config{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(context.Background())
	defer stop()
	sink := newTestSink()
	receiver, err := server.New(server.Config{
		Session:     server.SessionConfig{AllowNoAuth: true},
		MaxSessions: 1,
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- receiver.Serve(serveCtx, listener) }()

	clientTLS, err := transport.PinnedClientTLS(pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := transport.NewDialer("quic", transport.Config{TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{Dialer: dialer, Address: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := client.Upload(context.Background(), testBackup(testLayer(t, []byte("QUIC remote backup")))); err != nil {
		t.Fatal(err)
	} else if result.Digest != "sha256:done" {
		t.Fatalf("digest = %q", result.Digest)
	}
	if sink.commits.Load() != 1 {
		t.Fatalf("commits = %d", sink.commits.Load())
	}
	stop()
	select {
	case serveErr := <-serveDone:
		if !errors.Is(serveErr, context.Canceled) {
			t.Fatalf("server error = %v", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("QUIC server did not stop")
	}
}

func TestClientDoesNotRetryAuthenticationError(t *testing.T) {
	sink := newTestSink()
	dialer := &sessionDialer{cfg: server.SessionConfig{AuthToken: []byte("right")}, sink: sink}
	client, _ := New(Config{Dialer: dialer, Address: "pipe", AuthToken: "wrong", Backoffs: []time.Duration{0, 0}})
	result, err := client.Upload(context.Background(), testBackup(testLayer(t, []byte("x"))))
	var remoteErr *Error
	if !errors.As(err, &remoteErr) || remoteErr.Kind != server.ErrorAuth {
		t.Fatalf("error = %v", err)
	}
	if result.Attempts != 1 || dialer.attempts.Load() != 1 {
		t.Fatalf("attempts = %#v / %d", result, dialer.attempts.Load())
	}
}

func TestClientRetriesConnectionFailure(t *testing.T) {
	sink := newTestSink()
	dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true}, sink: sink, fail: 1}
	client, _ := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{0}})
	result, err := client.Upload(context.Background(), testBackup(testLayer(t, []byte("x"))))
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempts != 2 || dialer.attempts.Load() != 2 {
		t.Fatalf("attempts = %#v / %d", result, dialer.attempts.Load())
	}
}

func TestClientTokenRefresh(t *testing.T) {
	base := testLayer(t, bytes.Repeat([]byte("x"), 9<<20))
	layer := &slowLayer{Layer: base, delay: 450 * time.Millisecond}
	sink := newTokenSink()
	dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true}, sink: sink}
	provider := &rotatingProvider{}
	client, _ := New(Config{
		Dialer: dialer, Address: "pipe", Provider: provider,
		Backoffs: []time.Duration{}, Keepalive: 100 * time.Millisecond,
	})
	if _, err := client.Upload(context.Background(), testBackup(layer)); err != nil {
		t.Fatal(err)
	}
	if got := sink.tokens.Load(); got < 2 {
		t.Fatalf("token deliveries = %d, want refresh", got)
	}
	if got := provider.gets.Load(); got < 2 {
		t.Fatalf("provider calls = %d, want refresh", got)
	}
}

func TestClientValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("nil dialer accepted")
	}
	if _, err := New(Config{Dialer: &sessionDialer{}, Address: ""}); err == nil {
		t.Fatal("empty address accepted")
	}
	c, _ := New(Config{Dialer: &sessionDialer{}, Address: "pipe", Backoffs: []time.Duration{}})
	if _, err := c.Upload(context.Background(), Backup{}); err == nil {
		t.Fatal("empty backup accepted")
	}
	one := testLayer(t, []byte("x"))
	bad := testBackup(one)
	bad.Start.LayerCount = 2
	if _, err := c.Upload(context.Background(), bad); err == nil {
		t.Fatal("layer count mismatch accepted")
	}
	first := SessionID("repo", []byte("m"))
	second := SessionID("repo", []byte("m"))
	other := SessionID("other", []byte("m"))
	if first != second || first == other {
		t.Fatal("session ID is not deterministic and scoped")
	}
}

type sessionDialer struct {
	cfg      server.SessionConfig
	sink     server.Sink
	fail     int32
	attempts atomic.Int32
}

func (d *sessionDialer) Name() string { return "pipe" }
func (d *sessionDialer) Dial(ctx context.Context, _ string) (transport.Stream, error) {
	attempt := d.attempts.Add(1)
	if attempt <= d.fail {
		return nil, errors.New("injected dial failure")
	}
	session, err := server.NewSession(d.cfg, d.sink)
	if err != nil {
		return nil, err
	}
	client, peer := net.Pipe()
	go func() { _ = session.Run(ctx, peer) }()
	return client, nil
}

type testSink struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	opens   atomic.Int32
	commits atomic.Int32
}

func newTestSink() *testSink                                             { return &testSink{blobs: map[string][]byte{}} }
func (s *testSink) KnownBlobs(context.Context, string) ([]string, error) { return nil, nil }
func (s *testSink) BlobExists(_ context.Context, _, digest string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[digest]
	return ok, nil
}
func (s *testSink) OpenBlob(_ context.Context, _, digest string, _ int64) (server.BlobWriter, error) {
	s.opens.Add(1)
	return &testWriter{sink: s, digest: digest}, nil
}
func (s *testSink) CommitBackup(_ context.Context, _ server.Backup) (string, error) {
	s.commits.Add(1)
	return "sha256:done", nil
}

type testWriter struct {
	sink   *testSink
	digest string
	bytes.Buffer
}

func (w *testWriter) Commit(context.Context) error {
	w.sink.mu.Lock()
	w.sink.blobs[w.digest] = append([]byte(nil), w.Bytes()...)
	w.sink.mu.Unlock()
	return nil
}
func (w *testWriter) Abort(context.Context) error { return nil }

type tokenSink struct {
	*testSink
	tokens atomic.Int32
}

func newTokenSink() *tokenSink { return &tokenSink{testSink: newTestSink()} }
func (s *tokenSink) TokenScope(string) (string, []string, error) {
	return "me/repo", []string{"pull", "push"}, nil
}
func (s *tokenSink) ProvideToken(token *protocol.Token) {
	if token != nil && token.Value != "" {
		s.tokens.Add(1)
	}
}

type rotatingProvider struct{ gets atomic.Int32 }

func (p *rotatingProvider) Get(_ context.Context, scope registry.Scope) (*registry.Token, error) {
	n := p.gets.Add(1)
	return &registry.Token{Value: fmt.Sprintf("token-%d", n), ExpiresAt: time.Now().Add(2 * time.Second), Scope: scope}, nil
}
func (p *rotatingProvider) Invalidate(registry.Scope) {}

type slowLayer struct {
	v1.Layer
	delay time.Duration
}

func (l *slowLayer) Compressed() (io.ReadCloser, error) {
	r, err := l.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	return &slowReadCloser{ReadCloser: r, delay: l.delay}, nil
}

type slowReadCloser struct {
	io.ReadCloser
	delay time.Duration
}

func (r *slowReadCloser) Read(p []byte) (int, error) {
	time.Sleep(r.delay)
	return r.ReadCloser.Read(p)
}

func testLayer(t *testing.T, data []byte) v1.Layer {
	t.Helper()
	codec, _ := compress.Get("store")
	layer, err := ociimg.NewLayer([]ociimg.LayerFile{{
		Path: "/blobs/0", Mode: 0o644, Size: int64(len(data)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}}, codec, 0)
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

func testBackup(layer v1.Layer) Backup {
	size, _ := layer.Size()
	return Backup{
		Start: &protocol.BackupStart{
			Reference: "registry.test/me/repo:t", ManifestJson: []byte("{}"),
			MetadataLayer: []byte("meta"), LayerCount: 1, EstimatedBytes: uint64(size),
		},
		Layers: []v1.Layer{layer},
	}
}

func boolIntRemote(v bool) int {
	if v {
		return 1
	}
	return 0
}
func bool32(v bool) uint32 { return uint32(boolIntRemote(v)) }
