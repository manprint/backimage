package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// faultyAcceptor fails the first len(errs) accepts with errs, in order, then
// accepts from the real listener.
type faultyAcceptor struct {
	*net.TCPListener
	mu   sync.Mutex
	errs []error
}

func (a *faultyAcceptor) AcceptTCP() (*net.TCPConn, error) {
	a.mu.Lock()
	if len(a.errs) > 0 {
		err := a.errs[0]
		a.errs = a.errs[1:]
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()
	return a.TCPListener.AcceptTCP()
}

func acceptError(errno syscall.Errno) error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", errno)}
}

func faultyTCPListener(t *testing.T, errs ...error) (*tcpListener, string) {
	t.Helper()
	cert, pin, err := SelfSignedCertificate(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := tls13Config(&tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{TLS: tlsCfg, IdleTimeout: time.Minute}
	setDefaults(&cfg)
	l := startTCPListener(&faultyAcceptor{TCPListener: inner, errs: errs}, cfg)
	t.Cleanup(func() { _ = l.Close() })
	return l, pin
}

// TestAcceptSurvivesTransientErrors is the regression for the listener that
// went deaf: the first accept error that was not a timeout — EMFILE under
// descriptor pressure, a connection reset before accept — ended the accept
// loop without a word, and every later Accept waited forever on a port that
// was still open.
func TestAcceptSurvivesTransientErrors(t *testing.T) {
	l, pin := faultyTCPListener(t,
		acceptError(syscall.EMFILE), acceptError(syscall.ENFILE),
		acceptError(syscall.ECONNABORTED), acceptError(syscall.ENOBUFS),
		acceptError(syscall.EPROTO), acceptError(syscall.EPERM))

	clientTLS, err := PinnedClientTLS(pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDialer("tcp", Config{TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	dialed := make(chan error, 1)
	go func() {
		conn, err := d.Dial(context.Background(), l.Addr().String())
		if err == nil {
			_ = conn.Close()
		}
		dialed <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("accept after transient errors: %v", err)
	}
	_ = conn.Close()
	if err := <-dialed; err != nil {
		t.Fatalf("client: %v", err)
	}
}

// TestAcceptReportsAFatalError: an accept error that retrying cannot cure
// stops the listener and reaches Accept, wrapped as a closed listener so the
// server leaves its loop instead of spinning, and naming the cause.
func TestAcceptReportsAFatalError(t *testing.T) {
	l, _ := faultyTCPListener(t, acceptError(syscall.EINVAL))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := l.Accept(ctx)
	if !errors.Is(err, net.ErrClosed) || !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("Accept = %v, want a closed listener caused by EINVAL", err)
	}
	if _, err := l.Accept(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second Accept = %v, want the listener to stay closed", err)
	}
}

// TestQUICAcceptReportsAStoppedListener: the QUIC accept loop ended the same
// way when the underlying listener failed, leaving Accept blocked.
func TestQUICAcceptReportsAStoppedListener(t *testing.T) {
	cert, _, err := SelfSignedCertificate(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ln := mustListener(t, "quic", &tls.Config{Certificates: []tls.Certificate{cert}})
	ql := ln.(*quicListener)
	// The transport goes away under the loop, not through Close.
	if err := ql.listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ln.Accept(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept = %v, want the stopped listener reported as closed", err)
	}
}
