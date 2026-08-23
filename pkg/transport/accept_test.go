package transport

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// TestAcceptDoesNotStallOnASilentPeer covers the accept path against a peer
// that completes the transport handshake and then says nothing. Both
// transports finish their handshake inside Accept, so a blocking one lets a
// single connection hold the server's accept loop and starve every other
// client.
func TestAcceptDoesNotStallOnASilentPeer(t *testing.T) {
	for _, transportName := range []string{"tcp", "quic"} {
		t.Run(transportName, func(t *testing.T) {
			cert, pin, err := SelfSignedCertificate(nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			ln := mustListener(t, transportName, &tls.Config{Certificates: []tls.Certificate{cert}})

			// A peer that opens the socket and then goes silent. For TCP it
			// never sends a ClientHello; for QUIC it never opens a stream.
			stall := dialSilent(t, transportName, ln.Addr().String(), pin)
			defer stall()

			// A well behaved client must still get through.
			clientTLS, err := PinnedClientTLS(pin, nil)
			if err != nil {
				t.Fatal(err)
			}
			d, err := NewDialer(transportName, Config{TLS: clientTLS})
			if err != nil {
				t.Fatal(err)
			}
			dialed := make(chan error, 1)
			go func() {
				conn, err := d.Dial(context.Background(), ln.Addr().String())
				if err != nil {
					dialed <- err
					return
				}
				defer conn.Close()
				// QUIC opens the stream lazily: write so the server sees it.
				_, err = conn.Write([]byte{0})
				dialed <- err
			}()

			accepted := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				conn, err := ln.Accept(ctx)
				if err != nil {
					accepted <- err
					return
				}
				_ = conn.Close()
				accepted <- nil
			}()

			select {
			case err := <-accepted:
				if err != nil {
					t.Fatalf("accept while a silent peer is connected: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("accept was blocked by a silent peer")
			}
			if err := <-dialed; err != nil {
				t.Fatalf("legitimate client: %v", err)
			}
		})
	}
}

// dialSilent connects at the transport layer and then stops, without ever
// completing the handshake the server is waiting for.
func dialSilent(t *testing.T, transportName, addr, pin string) func() {
	t.Helper()
	if transportName == "tcp" {
		// A bare TCP connection: the TLS ClientHello never arrives.
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		return func() { _ = conn.Close() }
	}
	// QUIC: complete the handshake but never open a stream.
	clientTLS, err := PinnedClientTLS(pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := quicTLSConfig(clientTLS, addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, addr, cfg, quicConfig(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	return func() { _ = conn.CloseWithError(0, "") }
}
