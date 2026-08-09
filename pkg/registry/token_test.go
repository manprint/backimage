package registry

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
)

// tokenMux builds a registry that challenges with Bearer auth and mints
// tokens at /token (counting hits). The /data endpoint answers 401 unless
// a valid bearer token is attached.
func tokenMux(ttl int, noExpires bool, user, pass string) (srv *httptest.Server, tokHits, dataHits *atomic.Int64) {
	tokHits, dataHits = &atomic.Int64{}, &atomic.Int64{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token",service="t"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			tokHits.Add(1)
			if user != "" {
				u, p, ok := r.BasicAuth()
				if !ok || u != user || p != pass {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if noExpires {
				fmt.Fprint(w, `{"token":"tok"}`)
				return
			}
			fmt.Fprintf(w, `{"token":"tok-%d","expires_in":%d}`, tokHits.Load(), ttl)
		default:
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok") {
				dataHits.Add(1)
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	return srv, tokHits, dataHits
}

const scopeRepo = "me/dumps"

func TestTokenValid(t *testing.T) {
	now := time.Now()
	tok := &Token{Value: "x", ExpiresAt: now.Add(120 * time.Second)}
	if !tok.Valid(60 * time.Second) {
		t.Fatal("should be valid")
	}
	if tok.Valid(130 * time.Second) {
		t.Fatal("should be invalid")
	}
	if (*Token)(nil).Valid(time.Second) {
		t.Fatal("nil must be invalid")
	}
}

func TestTokenMintAndProactiveRefresh(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			n.Add(1)
			ttl := 300
			if n.Load() == 1 {
				ttl = 1 // first token dies fast, forcing a proactive refresh
			}
			fmt.Fprintf(w, `{"token":"t-%d","expires_in":%d}`, n.Load(), ttl)
		}
	}))
	defer srv.Close()
	p := NewProvider(srv.URL, nil)
	scope := Scope{Repository: scopeRepo, Actions: []string{"pull"}}

	t1, err := p.Get(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Value != "t-1" {
		t.Fatalf("first token = %q", t1.Value)
	}
	time.Sleep(1100 * time.Millisecond) // ttl 1s: margin 60s forces a refresh
	t2, err := p.Get(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if t2 == t1 {
		t.Fatal("expected a fresh token after expiry")
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("token requests = %d, want 2", got)
	}
}

func TestProviderCoalescing(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			n.Add(1)
			time.Sleep(80 * time.Millisecond)
			w.Write([]byte(`{"token":"t","expires_in":60}`))
		}
	}))
	defer srv.Close()
	p := NewProvider(srv.URL, nil)
	scope := Scope{Repository: scopeRepo, Actions: []string{"push"}}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Get(context.Background(), scope); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := n.Load(); got != 1 {
		t.Fatalf("token requests = %d, want 1", got)
	}
}

func TestInvalidateForcesRefresh(t *testing.T) {
	srv, hits, _ := tokenMux(600, false, "", "")
	defer srv.Close()
	p := NewProvider(srv.URL, nil)
	scope := Scope{Repository: scopeRepo, Actions: []string{"pull"}}
	if _, err := p.Get(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	p.Invalidate(scope)
	if _, err := p.Get(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("token requests = %d, want 2", got)
	}
}

func TestTokenNoExpiresInAssumes60s(t *testing.T) {
	srv, _, _ := tokenMux(600, true, "", "")
	defer srv.Close()
	p := NewProvider(srv.URL, nil)
	tok, err := p.Get(context.Background(), Scope{Repository: "r"})
	if err != nil {
		t.Fatal(err)
	}
	left := time.Until(tok.ExpiresAt)
	if left > 61*time.Second || left < 59*time.Second {
		t.Fatalf("token ttl = %v, want ~60s", left)
	}
}

func TestBearerAuth401RetryOnce(t *testing.T) {
	var dataHits, tokenHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			tokenHits.Add(1)
			w.Write([]byte(`{"token":"tok","expires_in":600}`))
		default:
			if dataHits.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized) // token considered stale
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("done"))
		}
	}))
	defer srv.Close()
	p := NewProvider(srv.URL, nil)
	rt := NewRoundTripper(http.DefaultTransport, p, Scope{Repository: scopeRepo, Actions: []string{"pull"}})
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/data", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := dataHits.Load(); got != 2 {
		t.Fatalf("data hits = %d, want 2 (1 retry)", got)
	}
	if got := tokenHits.Load(); got != 2 {
		t.Fatalf("token requests = %d, want 2 (refresh after invalidation)", got)
	}
}

func TestBearerAuthNoRetryWithoutGetBody(t *testing.T) {
	var dataHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			w.Write([]byte(`{"token":"tok","expires_in":600}`))
		default:
			dataHits.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	p := NewProvider(srv.URL, nil)
	rt := NewRoundTripper(http.DefaultTransport, p, Scope{Repository: scopeRepo})
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPatch, srv.URL+"/blob", bytes.NewReader([]byte("x")))
	req.GetBody = nil // simulate a body that cannot be replayed
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 (body not replayable), got %d", resp.StatusCode)
	}
	if got := dataHits.Load(); got != 1 {
		t.Fatalf("data hits = %d, want 1 (no retry)", got)
	}
}

func TestBearerAuthDouble401FailsAfterOneRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			w.Write([]byte(`{"token":"t","expires_in":600}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	var hits atomic.Int64
	p := NewProvider(srv.URL, nil)
	rt := NewRoundTripper(&countingTransport{n: &hits, base: http.DefaultTransport}, p, Scope{Repository: "r"})
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/data", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("data requests = %d, want 2 (1 retry)", got)
	}
}

type countingTransport struct {
	n    *atomic.Int64
	base http.RoundTripper
}

func (ct *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ct.n.Add(1)
	return ct.base.RoundTrip(req)
}

func TestProviderBasicAuthPassedToToken(t *testing.T) {
	srv, _, _ := tokenMux(600, false, "me", "pw")
	defer srv.Close()
	auth := authn.FromConfig(authn.AuthConfig{Username: "me", Password: "pw"})
	p := NewProvider(srv.URL, auth)
	tok, err := p.Get(context.Background(), Scope{Repository: "repo/x", Actions: []string{"push", "pull"}})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value == "" {
		t.Fatal("empty token")
	}
}
