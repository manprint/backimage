package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestIdleTimeoutIsIdleNotAbsolute pins the meaning of Config.IdleTimeout: a
// connection that keeps transferring must survive past the timeout. A backup
// stream is long lived by construction, so an absolute deadline set once at
// dial time would cap every remote backup at IdleTimeout regardless of how
// much progress it is making.
func TestIdleTimeoutIsIdleNotAbsolute(t *testing.T) {
	const idle = 300 * time.Millisecond
	cert, pin, err := SelfSignedCertificate(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := NewListener("tcp", "127.0.0.1:0", Config{
		TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, IdleTimeout: idle,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		// Echo back whatever arrives until the client stops writing.
		_, err = io.Copy(io.Discard, conn)
		serverErr <- err
	}()

	clientTLS, err := PinnedClientTLS(pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDialer("tcp", Config{TLS: clientTLS, IdleTimeout: idle})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Write steadily for four times the idle timeout. Every write is well
	// inside the idle window, so none of them may fail.
	deadline := time.Now().Add(4 * idle)
	payload := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write after %v of continuous traffic: %v", idle, err)
		}
		time.Sleep(idle / 4)
	}
}

// TestIdleTimeoutStillFiresWhenIdle is the other half of the contract: a
// connection with no traffic at all must still be dropped.
func TestIdleTimeoutStillFiresWhenIdle(t *testing.T) {
	const idle = 250 * time.Millisecond
	cert, pin, err := SelfSignedCertificate(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := NewListener("tcp", "127.0.0.1:0", Config{
		TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, IdleTimeout: idle,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		// Hold the connection open without ever sending anything.
		time.Sleep(4 * idle)
		_ = conn.Close()
	}()

	clientTLS, err := PinnedClientTLS(pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDialer("tcp", Config{TLS: clientTLS, IdleTimeout: idle})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	buf := make([]byte, 16)
	start := time.Now()
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("read on a silent connection returned no error")
	} else {
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("read error = %v, want a timeout", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 3*idle {
		t.Fatalf("idle read blocked for %v, want about %v", elapsed, idle)
	}
}
