package registry

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// Tuning of the shared registry transport. The defaults of
// http.DefaultTransport are aimed at many small requests; a registry push is
// the opposite workload — few, very large, sequential request bodies — and
// three of those defaults dominate upload throughput. See
// docs/TROUGHPUT_IMPROVE.md.
const (
	// uploadWriteBufferSize replaces the 4 KiB default, which turns a single
	// multi-megabyte PATCH into thousands of write syscalls at gigabit rates.
	uploadWriteBufferSize = 256 << 10
	// uploadReadBufferSize is raised for symmetry; registry responses are
	// small, so this only matters for pulls.
	uploadReadBufferSize = 64 << 10
	// maxIdleConnsPerRegistry keeps one pooled connection per concurrent
	// upload job. The default is 2, below the default --jobs of 3, so one
	// connection was torn down and re-handshaked continuously. The value
	// covers the realistic --jobs range; above it, the extra connections
	// still work but are not pooled, which is the stdlib behaviour anyway.
	maxIdleConnsPerRegistry = 16
)

var (
	registryTransportOnce sync.Once
	registryTransportInst *http.Transport
)

// registryTransport returns the process-wide transport used for every
// registry request. It is shared, exactly like the http.DefaultTransport it
// replaces, so connection pooling still spans separate pushes and separate
// BlobClient instances.
//
// It differs from http.DefaultTransport on three points:
//
//   - HTTP/2 is disabled. Over HTTPS, DefaultTransport negotiates h2 and then
//     multiplexes every concurrent upload onto one TCP connection; registries
//     behind nginx or envoy advertise small per-stream flow-control windows,
//     so the uploads serialise and stall on WINDOW_UPDATE. On HTTP/1.1 each
//     job owns a real connection.
//   - The idle connection pool is sized for concurrent uploads.
//   - The socket buffers are sized for large bodies.
//
// Proxy resolution, dial and TLS handshake timeouts and the TLS defaults are
// inherited from http.DefaultTransport, so no security-relevant default
// changes here.
func registryTransport() *http.Transport {
	registryTransportOnce.Do(func() {
		registryTransportInst = newRegistryTransport()
	})
	return registryTransportInst
}

// newRegistryClient builds the HTTP client every registry call site uses:
// the shared tuned transport, wrapped in bearer-token handling. Going through
// one constructor keeps a future call site from falling back to
// http.DefaultTransport and losing the tuning.
func newRegistryClient(p Provider, scope Scope) *http.Client {
	return &http.Client{Transport: NewRoundTripper(registryTransport(), p, scope)}
}

func newRegistryTransport() *http.Transport {
	t := baseTransportClone()
	// A non-nil TLSNextProto map disables the automatic HTTP/2 upgrade; both
	// this and ForceAttemptHTTP2 are needed, the first for an explicit
	// TLSClientConfig and the second for the automatic path.
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(authority string, c *tls.Conn) http.RoundTripper{}
	// Transport.Clone materializes a TLS configuration advertising
	// NextProtos ["h2", "http/1.1"]. Left alone, ALPN would still negotiate
	// h2 against a server that offers it, on a transport that has no h2
	// handler registered. Advertise HTTP/1.1 only.
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	if t.TLSClientConfig.MinVersion == 0 {
		// Same value the standard library already enforces for clients,
		// stated explicitly so it cannot silently regress.
		t.TLSClientConfig.MinVersion = tls.VersionTLS12
	}
	t.MaxIdleConnsPerHost = maxIdleConnsPerRegistry
	if t.MaxIdleConns > 0 && t.MaxIdleConns < maxIdleConnsPerRegistry {
		t.MaxIdleConns = maxIdleConnsPerRegistry
	}
	t.WriteBufferSize = uploadWriteBufferSize
	t.ReadBufferSize = uploadReadBufferSize
	return t
}

// baseTransportClone returns a copy of http.DefaultTransport, or an
// equivalent transport when the process replaced DefaultTransport with
// something that is not an *http.Transport.
func baseTransportClone() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
