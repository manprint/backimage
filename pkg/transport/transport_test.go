package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func TestHandshakeTLS13AndPin(t *testing.T) {
	for _, transportName := range []string{"tcp", "quic"} {
		t.Run(transportName, func(t *testing.T) {
			cert, pin, err := SelfSignedCertificate(nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			ln := mustListener(t, transportName, &tls.Config{Certificates: []tls.Certificate{cert}})
			accepted := make(chan Stream, 1)
			acceptErr := make(chan error, 1)
			go func() {
				conn, err := ln.Accept(context.Background())
				if err != nil {
					acceptErr <- err
					return
				}
				accepted <- conn
			}()

			clientTLS, err := PinnedClientTLS(pin, nil)
			if err != nil {
				t.Fatal(err)
			}
			d, err := NewDialer(transportName, Config{TLS: clientTLS})
			if err != nil {
				t.Fatal(err)
			}
			c, err := d.Dial(context.Background(), ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if transportName == "quic" {
				if _, err := c.Write([]byte{0}); err != nil {
					t.Fatal(err)
				}
			}
			var server Stream
			select {
			case server = <-accepted:
				defer server.Close()
			case err := <-acceptErr:
				t.Fatal(err)
			case <-time.After(2 * time.Second):
				t.Fatal("accept timed out")
			}
			if got := streamTLSVersion(c); got != tls.VersionTLS13 {
				t.Fatalf("TLS version = %x", got)
			}
			if d.Name() != transportName {
				t.Fatalf("name = %q", d.Name())
			}
		})
	}
}

func TestRejectsTLS12AndWrongPin(t *testing.T) {
	cert, _, err := SelfSignedCertificate(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, transportName := range []string{"tcp", "quic"} {
		t.Run(transportName+"/tls12", func(t *testing.T) {
			if _, err := NewDialer(transportName, Config{TLS: &tls.Config{MaxVersion: tls.VersionTLS12}}); err == nil {
				t.Fatal("TLS 1.2-only config accepted")
			}
		})
		t.Run(transportName+"/pin", func(t *testing.T) {
			ln := mustListener(t, transportName, &tls.Config{Certificates: []tls.Certificate{cert}})
			go func() {
				conn, _ := ln.Accept(context.Background())
				if conn != nil {
					_ = conn.Close()
				}
			}()
			badPin := hex.EncodeToString(make([]byte, sha256.Size))
			cfg, err := PinnedClientTLS(badPin, nil)
			if err != nil {
				t.Fatal(err)
			}
			d, err := NewDialer(transportName, Config{TLS: cfg})
			if err != nil {
				t.Fatal(err)
			}
			if conn, err := d.Dial(context.Background(), ln.Addr().String()); err == nil {
				_ = conn.Close()
				t.Fatal("wrong pin accepted")
			}
		})
	}
}

func TestMutualTLS(t *testing.T) {
	ca, server, client := makeTestPKI(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	serverCfg := &tls.Config{
		Certificates: []tls.Certificate{server},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}

	for _, transportName := range []string{"tcp", "quic"} {
		t.Run(transportName+"/client certificate", func(t *testing.T) {
			ln := mustListener(t, transportName, serverCfg)
			accepted := make(chan error, 1)
			go func() {
				conn, err := ln.Accept(context.Background())
				if conn != nil {
					_ = conn.Close()
				}
				accepted <- err
			}()
			pin := fingerprint(server.Certificate[0])
			clientCfg, _ := PinnedClientTLS(pin, &client)
			d, _ := NewDialer(transportName, Config{TLS: clientCfg})
			conn, err := d.Dial(context.Background(), ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if transportName == "quic" {
				if _, err := conn.Write([]byte{0}); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-accepted; err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		})

		t.Run(transportName+"/missing client certificate", func(t *testing.T) {
			ln := mustListener(t, transportName, serverCfg)
			serverErr := make(chan error, 1)
			acceptCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			go func() { _, err := ln.Accept(acceptCtx); serverErr <- err }()
			clientCfg, _ := PinnedClientTLS(fingerprint(server.Certificate[0]), nil)
			d, _ := NewDialer(transportName, Config{TLS: clientCfg})
			conn, err := d.Dial(context.Background(), ln.Addr().String())
			if err == nil && transportName == "tcp" {
				// With TLS 1.3 the client can finish its side of the handshake before
				// observing the server's fatal alert. The first read must observe it.
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var one [1]byte
				if _, readErr := conn.Read(one[:]); readErr == nil {
					t.Fatal("mTLS connection remained usable without a client certificate")
				}
			}
			if conn != nil {
				_ = conn.Close()
			}
			if acceptErr := <-serverErr; acceptErr == nil {
				t.Fatal("server did not reject missing client certificate")
			}
		})
	}
}

func TestListenerCancellationAndConfigValidation(t *testing.T) {
	cert, _, _ := SelfSignedCertificate(nil, time.Now())
	for _, transportName := range []string{"tcp", "quic"} {
		t.Run(transportName, func(t *testing.T) {
			ln := mustListener(t, transportName, &tls.Config{Certificates: []tls.Certificate{cert}})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := ln.Accept(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Accept error = %v", err)
			}
			if _, err := NewDialer(transportName, Config{}); err == nil {
				t.Fatal("nil TLS config accepted")
			}
			if _, err := NewListener(transportName, "127.0.0.1:0", Config{TLS: &tls.Config{}}); err == nil {
				t.Fatal("missing server certificate accepted")
			}
		})
	}
	if _, err := NewDialer("missing", Config{}); err == nil {
		t.Fatal("unknown dialer accepted")
	}
	if _, err := NewListener("missing", "", Config{}); err == nil {
		t.Fatal("unknown listener accepted")
	}
	if _, err := PinnedClientTLS("not-a-pin", nil); err == nil {
		t.Fatal("invalid pin accepted")
	}
	if err := Register("tcp", newTCPDialer, newTCPListener); err == nil {
		t.Fatal("duplicate transport accepted")
	}
}

func TestQUICRejectsWrongALPNAndSupportsMinimumMTU(t *testing.T) {
	cert, _, err := SelfSignedCertificate(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ln := mustListener(t, "quic", &tls.Config{Certificates: []tls.Certificate{cert}})
	wrongALPN := &tls.Config{ // #nosec G402 -- negative protocol test.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"wrong/1"},
	}
	d, err := quic.DialAddr(context.Background(), ln.Addr().String(), wrongALPN, quicConfig(Config{}))
	if err == nil {
		_ = d.CloseWithError(0, "")
		t.Fatal("wrong ALPN accepted")
	}

	accepted := make(chan Stream, 1)
	go func() {
		conn, acceptErr := ln.Accept(context.Background())
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	pin := fingerprint(cert.Certificate[0])
	clientTLS, err := PinnedClientTLS(pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := NewDialer("quic", Config{TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	client, err := dialer.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload := make([]byte, 128<<10)
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	defer server.Close()
	buf := make([]byte, len(payload))
	if _, err := server.Read(buf); err != nil {
		t.Fatal(err)
	}
}

func TestQUICConfigurationAndConnectionClose(t *testing.T) {
	if _, err := newQUICDialer(Config{TLS: &tls.Config{}, QUICStreams: -1}); err == nil {
		t.Fatal("negative QUIC stream limit accepted by dialer")
	}
	cert, pin, err := SelfSignedCertificate(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newQUICListener("127.0.0.1:0", Config{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, QUICStreams: -1}); err == nil {
		t.Fatal("negative QUIC stream limit accepted by listener")
	}

	cfg, err := quicTLSConfig(&tls.Config{}, "127.0.0.1:7575")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "127.0.0.1" || len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != quicALPN {
		t.Fatalf("QUIC TLS config = %#v", cfg)
	}
	pinned, err := PinnedClientTLS(pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = quicTLSConfig(pinned, "127.0.0.1:7575"); err != nil || cfg.ServerName != "" {
		t.Fatalf("pinned QUIC TLS config = %#v, %v", cfg, err)
	}

	quicCfg := quicConfig(Config{QUICStreams: 2, QUICWindow: 1 << 20})
	if quicCfg.MaxIncomingStreams != 2 || quicCfg.InitialStreamReceiveWindow != 1<<20 || quicCfg.MaxConnectionReceiveWindow != 8<<20 {
		t.Fatalf("QUIC config = %#v", quicCfg)
	}

	ln := mustListener(t, "quic", &tls.Config{Certificates: []tls.Certificate{cert}})
	quicListener, ok := ln.(*quicListener)
	if !ok || quicListener.Name() != "quic" {
		t.Fatalf("listener = %#v", ln)
	}
	accepted := make(chan Stream, 1)
	go func() {
		stream, acceptErr := ln.Accept(context.Background())
		if acceptErr == nil {
			accepted <- stream
		}
	}()
	dialer, err := NewDialer("quic", Config{TLS: pinned})
	if err != nil {
		t.Fatal(err)
	}
	client, err := dialer.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	defer server.Close()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	closer, ok := client.(ConnectionCloser)
	if !ok {
		t.Fatal("QUIC stream does not expose ConnectionCloser")
	}
	if err := closer.CloseConnection(); err != nil {
		t.Fatal(err)
	}
	if err := closer.CloseConnection(); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dialer.Dial(canceled, "127.0.0.1:1"); err == nil {
		t.Fatal("canceled QUIC dial accepted")
	}
}

func mustListener(t *testing.T, transportName string, tlsCfg *tls.Config) Listener {
	t.Helper()
	ln, err := NewListener(transportName, "127.0.0.1:0", Config{TLS: tlsCfg, IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func streamTLSVersion(stream Stream) uint16 {
	switch conn := stream.(type) {
	case *tls.Conn:
		return conn.ConnectionState().Version
	case *quicStream:
		return conn.conn.ConnectionState().TLS.Version
	default:
		return 0
	}
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func makeTestPKI(t *testing.T) (*x509.Certificate, tls.Certificate, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(serial int64, eku x509.ExtKeyUsage) tls.Certificate {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: "localhost"},
			DNSNames:     []string{"localhost"},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}
	}
	return ca, issue(2, x509.ExtKeyUsageServerAuth), issue(3, x509.ExtKeyUsageClientAuth)
}
