package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

// tlsServerWithHTTP2 starts an HTTPS test server that offers HTTP/2 via ALPN
// and echoes the protocol version it served the request with.
func tlsServerWithHTTP2(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-Proto", r.Proto)
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// trustServer returns a copy of tr that trusts srv's certificate. Only RootCAs
// is touched: the ALPN protocol list must survive, otherwise the test would
// negotiate on a configuration that production never uses.
func trustServer(tr *http.Transport, srv *httptest.Server) *http.Transport {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	clone := tr.Clone()
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	clone.TLSClientConfig.RootCAs = pool
	return clone
}

func servedProto(t *testing.T, tr *http.Transport, url string) string {
	t.Helper()
	client := &http.Client{Transport: tr}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("draining body: %v", err)
	}
	return resp.Header.Get("X-Served-Proto")
}

// TestRegistryTransportNegotiatesHTTP11 is the regression guard for the upload
// stall: against a server that offers HTTP/2, the stock transport negotiates
// h2 and would multiplex every concurrent upload onto one connection, while
// the registry transport must stay on HTTP/1.1.
func TestRegistryTransportNegotiatesHTTP11(t *testing.T) {
	srv := tlsServerWithHTTP2(t)

	if got := servedProto(t, trustServer(baseTransportClone(), srv), srv.URL); got != "HTTP/2.0" {
		t.Fatalf("control case: stock transport served %q, want HTTP/2.0; the test no longer proves anything", got)
	}
	if got := servedProto(t, trustServer(registryTransport(), srv), srv.URL); got != "HTTP/1.1" {
		t.Errorf("registry transport served %q, want HTTP/1.1", got)
	}
}

func TestRegistryTransportIsTunedAndShared(t *testing.T) {
	tr := registryTransport()
	if tr != registryTransport() {
		t.Error("registryTransport must return one shared transport so the connection pool spans calls")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 must be off")
	}
	if tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto must be a non-nil empty map to disable the HTTP/2 upgrade, got %v", tr.TLSNextProto)
	}
	if tr.MaxIdleConnsPerHost != maxIdleConnsPerRegistry {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, maxIdleConnsPerRegistry)
	}
	if tr.MaxIdleConns > 0 && tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns = %d, below the per-host pool %d", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.WriteBufferSize != uploadWriteBufferSize {
		t.Errorf("WriteBufferSize = %d, want %d", tr.WriteBufferSize, uploadWriteBufferSize)
	}
	if tr.ReadBufferSize != uploadReadBufferSize {
		t.Errorf("ReadBufferSize = %d, want %d", tr.ReadBufferSize, uploadReadBufferSize)
	}
	// Transport.Clone materializes NextProtos ["h2", "http/1.1"]; leaving it
	// there would let ALPN pick h2 on a transport with no h2 handler.
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig must exist to constrain the advertised ALPN protocols")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Errorf("advertised ALPN protocols = %v, want [http/1.1]", got)
	}
}

// TestRegistryTransportInheritsDefaults pins the fields that must keep coming
// from http.DefaultTransport: proxy resolution and the handshake timeouts are
// security- and environment-relevant and are not ours to redefine.
func TestRegistryTransportInheritsDefaults(t *testing.T) {
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport is not an *http.Transport in this process")
	}
	tr := registryTransport()
	if (tr.Proxy == nil) != (def.Proxy == nil) {
		t.Error("proxy resolution must be inherited from http.DefaultTransport")
	}
	if tr.TLSHandshakeTimeout != def.TLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want the default %v", tr.TLSHandshakeTimeout, def.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout != def.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want the default %v", tr.IdleConnTimeout, def.IdleConnTimeout)
	}
	if tr.ExpectContinueTimeout != def.ExpectContinueTimeout {
		t.Errorf("ExpectContinueTimeout = %v, want the default %v", tr.ExpectContinueTimeout, def.ExpectContinueTimeout)
	}
	// The transport constrains ALPN, and nothing else about TLS: verification
	// and the trust store stay exactly as the standard library sets them.
	if cfg := tr.TLSClientConfig; cfg != nil {
		if cfg.InsecureSkipVerify {
			t.Error("certificate verification must stay on")
		}
		if cfg.RootCAs != nil {
			t.Error("the transport must keep using the system trust store")
		}
		if cfg.MinVersion < tls.VersionTLS12 {
			t.Errorf("MinVersion = %#x, want at least TLS 1.2", cfg.MinVersion)
		}
	}
}

// TestRegistryClientsUseSharedTransport checks the wiring: a client built by
// the package must reach the tuned transport through the bearer round tripper.
func TestRegistryClientsUseSharedTransport(t *testing.T) {
	assertShared := func(t *testing.T, rt http.RoundTripper) {
		t.Helper()
		auth, ok := rt.(*bearerAuth)
		if !ok {
			t.Fatalf("transport is %T, want *bearerAuth", rt)
		}
		if auth.base != http.RoundTripper(registryTransport()) {
			t.Errorf("bearer round tripper wraps %T, want the shared registry transport", auth.base)
		}
	}

	t.Run("blob client", func(t *testing.T) {
		ref, err := name.ParseReference("localhost:5000/repo:tag", name.WeakValidation)
		if err != nil {
			t.Fatalf("parsing reference: %v", err)
		}
		c, err := NewBlobClient(context.Background(), ref, testTokenProvider(), 1<<20)
		if err != nil {
			t.Fatalf("NewBlobClient: %v", err)
		}
		assertShared(t, c.p.client.Transport)
	})

	t.Run("constructor", func(t *testing.T) {
		assertShared(t, newRegistryClient(testTokenProvider(), Scope{Repository: "repo"}).Transport)
	})
}

func testTokenProvider() Provider {
	return NewStaticProvider(func(context.Context, Scope) (*Token, error) {
		return &Token{Value: "tok"}, nil
	})
}
