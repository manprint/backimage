package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/transport"
)

func TestServerConcurrencyLimit(t *testing.T) {
	metrics := new(Metrics)
	srv, err := New(Config{
		Session:     SessionConfig{AllowNoAuth: true, IdleTimeout: time.Second},
		MaxSessions: 1, Metrics: metrics,
	}, newMemorySink())
	if err != nil {
		t.Fatal(err)
	}
	listener := newChannelListener()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, listener) }()

	client1, server1 := net.Pipe()
	listener.conns <- server1
	writeHello(t, client1, "", protocol.Version)
	if ack := readServer(t, client1).GetHelloAck(); ack == nil {
		t.Fatal("first session was not accepted")
	}

	client2, server2 := net.Pipe()
	listener.conns <- server2
	rejected := readServer(t, client2).GetError()
	if rejected == nil || !strings.Contains(rejected.Message, "maximum concurrent sessions") {
		t.Fatalf("rejection = %v", rejected)
	}
	_ = client2.Close()

	cancel()
	_ = client1.Close()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve error = %v", err)
	}
	if metrics.errorsNetwork.Load() != 1 {
		t.Fatalf("network errors = %d", metrics.errorsNetwork.Load())
	}
}

func TestMetricsHealthAndPrometheusOutput(t *testing.T) {
	m := new(Metrics)
	m.sessionStarted()
	m.addReceived(12)
	m.addUploaded(10)
	m.addSkipped()
	m.addError(ErrorAuth)
	m.addError(ErrorUsage)
	m.addError(ErrorIntegrity)
	m.addError(ErrorNetwork)
	m.addError(ErrorGeneric)
	m.sessionDone(1500 * time.Millisecond)

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/healthz", "ok\n"},
		{"/metrics", "backimage_bytes_received_total 12\n"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: status=%d body=%q", tc.path, rec.Code, rec.Body.String())
		}
	}
	body := func() string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}()
	for _, want := range []string{
		"backimage_sessions_active 0",
		"backimage_sessions_total 1",
		"backimage_bytes_uploaded_total 10",
		"backimage_layers_skipped_total 1",
		"backimage_errors_total{kind=\"auth\"} 1",
		"backimage_session_duration_seconds_sum 1.500000000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestServerValidation(t *testing.T) {
	if _, err := New(Config{}, nil); err == nil {
		t.Fatal("nil sink accepted")
	}
	if _, err := New(Config{Session: SessionConfig{}}, newMemorySink()); err == nil {
		t.Fatal("server without authentication accepted")
	}
	srv, err := New(Config{Session: SessionConfig{AllowNoAuth: true}}, newMemorySink())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(context.Background(), nil); err == nil {
		t.Fatal("nil listener accepted")
	}
	if srv.Metrics() == nil {
		t.Fatal("server metrics missing")
	}
	called := false
	srv.cfg.OnError = func(error) { called = true }
	srv.report(errors.New("reported"))
	if !called {
		t.Fatal("OnError was not called")
	}
}

type channelListener struct {
	conns  chan transport.Stream
	closed chan struct{}
	once   sync.Once
}

func newChannelListener() *channelListener {
	return &channelListener{conns: make(chan transport.Stream, 4), closed: make(chan struct{})}
}

func (l *channelListener) Accept(ctx context.Context) (transport.Stream, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *channelListener) Addr() net.Addr { return testAddr("test") }
func (l *channelListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }
