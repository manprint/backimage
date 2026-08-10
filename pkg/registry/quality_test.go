package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

func testReference(t *testing.T, rawURL string) name.Reference {
	t.Helper()
	ref, err := name.ParseReference(strings.TrimPrefix(rawURL, "http://") + "/me/backup:latest")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestVerifyPushAccessAuthModes(t *testing.T) {
	t.Run("anonymous", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		if err := VerifyPushAccess(context.Background(), testReference(t, srv.URL), nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("basic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != "me" || p != "pw" {
				w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/v2/" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		creds := Credentials{Registry: strings.TrimPrefix(srv.URL, "http://"), Username: "me", Secret: "pw"}
		if err := VerifyPushAccess(context.Background(), testReference(t, srv.URL), NewKeychain(&creds, nil)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		var tokenHits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v2/":
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token",service="test"`)
				w.WriteHeader(http.StatusUnauthorized)
			case "/token":
				tokenHits.Add(1)
				fmt.Fprint(w, `{"token":"fresh","expires_in":600}`)
			default:
				if r.Header.Get("Authorization") != "Bearer fresh" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()
		if err := VerifyPushAccess(context.Background(), testReference(t, srv.URL), nil); err != nil {
			t.Fatal(err)
		}
		if tokenHits.Load() != 1 {
			t.Fatalf("token requests = %d", tokenHits.Load())
		}
	})

	t.Run("rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		if err := VerifyPushAccess(context.Background(), testReference(t, srv.URL), nil); err == nil {
			t.Fatal("expected rejected push access")
		}
	})
}

func TestReadyBearerTokenCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ready" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	creds := TokenCredentials(host, "ready")
	if err := VerifyCredentials(context.Background(), host, creds); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPushAccess(context.Background(), testReference(t, srv.URL), NewKeychain(&creds, nil)); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(creds); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(host)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewKeychain(nil, store).Resolve(fakeRes(host))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.Authorization()
	if err != nil || cfg.RegistryToken != "ready" || loaded.Secret != "ready" {
		t.Fatalf("stored token was not reconstructed: cfg=%+v loaded=%+v err=%v", cfg, loaded, err)
	}
}

func TestReadyBearerTokenRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	if err := VerifyCredentials(context.Background(), host, TokenCredentials(host, "bad")); err == nil || !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("ready token rejection = %v", err)
	}
}

func TestVerifyCredentialFallbackAndHelpers(t *testing.T) {
	var tokenHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token",service="test"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			tokenHits.Add(1)
			if r.URL.Query().Get("scope") != "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `{"token":"fallback"}`)
		}
	}))
	defer srv.Close()
	if err := VerifyCredentials(context.Background(), srv.URL, Credentials{Username: "u", Secret: "p"}); err != nil {
		t.Fatal(err)
	}
	if tokenHits.Load() != 2 {
		t.Fatalf("token fallback hits = %d", tokenHits.Load())
	}

	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		t.Run(fmt.Sprintf("status-%d", status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
				}
				w.WriteHeader(status)
			}))
			defer s.Close()
			if err := VerifyCredentials(context.Background(), s.URL, Credentials{}); err == nil {
				t.Fatal("unexpected registry response accepted")
			}
		})
	}

	base, _ := url.Parse("https://registry.test/v2/")
	if resolveRealm("https://auth.test/token", base) != "https://auth.test/token" ||
		resolveRealm("", base) != "" || resolveRealm("auth.test/token", base) != "https://auth.test/token" {
		t.Fatal("realm resolution failed")
	}
	if got, err := httpBase("localhost:5000"); err != nil || got != "http://localhost:5000" {
		t.Fatalf("localhost base = %q, %v", got, err)
	}
	if _, err := httpBase("ftp://example.test"); err == nil {
		t.Fatal("unsupported registry scheme accepted")
	}
}

func TestProviderCoalescesFailures(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits.Add(1)
		time.Sleep(40 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := NewProvider(srv.URL, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Get(context.Background(), Scope{Repository: "r"}); err == nil {
				t.Error("expected shared mint error")
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("failed token requests = %d, want 1", hits.Load())
	}
}

func TestProviderTokenBodyVariants(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"issued-at", `{"token":"ok","expires_in":120,"issued_at":"2030-01-02T03:04:05Z"}`, false},
		{"empty", `{"token":"","expires_in":120}`, true},
		{"malformed", `{`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			tok, err := NewProvider(srv.URL, nil).Get(context.Background(), Scope{Repository: "r"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Get = %+v, %v", tok, err)
			}
			if !tc.wantErr && !tok.ExpiresAt.Equal(time.Date(2030, 1, 2, 3, 6, 5, 0, time.UTC)) {
				t.Fatalf("issued_at not applied: %v", tok.ExpiresAt)
			}
		})
	}
}

func TestStaticProviderAndReplayable401Body(t *testing.T) {
	var tokenCalls atomic.Int64
	p := NewStaticProvider(func(context.Context, Scope) (*Token, error) {
		n := tokenCalls.Add(1)
		return &Token{Value: fmt.Sprintf("t%d", n), ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	var bodies []string
	var mu sync.Mutex
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		status := http.StatusOK
		if n == 1 {
			status = http.StatusUnauthorized
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
	})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPatch, "http://example.test/blob", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewRoundTripper(base, p, Scope{Repository: "r"}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(bodies) != 2 || bodies[0] != "payload" || bodies[1] != "payload" {
		t.Fatalf("request body was not replayed: %v", bodies)
	}
	if tokenCalls.Load() != 2 {
		t.Fatalf("token calls = %d", tokenCalls.Load())
	}

	bad := NewStaticProvider(nil)
	if _, err := bad.Get(context.Background(), Scope{}); err == nil {
		t.Fatal("nil static provider must fail safely")
	}
	bad.Invalidate(Scope{})
	defaultRT := NewRoundTripper(nil, p, Scope{}).(*bearerAuth)
	if defaultRT.base != http.DefaultTransport {
		t.Fatal("nil base transport did not select http.DefaultTransport")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckpointStoreValidationAndCorruption(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	store := NewCheckpointStore("")
	ck := &Checkpoint{ID: "safe-id_1", Ref: "example.test/r:t", DoneBlobs: []string{"sha256:a"}}
	if err := store.Save(ck); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ck.ID)
	if err != nil || got.Ref != ck.Ref || len(got.DoneBlobs) != 1 {
		t.Fatalf("load = %+v, %v", got, err)
	}
	if err := store.Delete(ck.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ck.ID); err != nil {
		t.Fatalf("deleting an absent checkpoint: %v", err)
	}
	if err := store.Delete("../escape"); err == nil {
		t.Fatal("invalid checkpoint id deleted")
	}
	if _, err := store.Load(ck.ID); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("missing checkpoint error = %v", err)
	}
	if err := store.Save(nil); err == nil {
		t.Fatal("nil checkpoint must fail")
	}
	if err := store.Save(&Checkpoint{ID: "../escape"}); err == nil {
		t.Fatal("invalid checkpoint id saved")
	}
	for _, id := range []string{"", "../escape", "bad/name"} {
		if _, err := store.Load(id); err == nil {
			t.Fatalf("invalid id %q accepted", id)
		}
	}

	dir := t.TempDir()
	corrupt := NewCheckpointStore(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := corrupt.Load("broken"); err == nil {
		t.Fatal("corrupt checkpoint must fail")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewCheckpointStore(filepath.Join(blocker, "child")).Save(&Checkpoint{ID: "x"}); err == nil {
		t.Fatal("checkpoint store under a file should fail")
	}
}

func TestKeychainPropagatesStoreError(t *testing.T) {
	kc := NewKeychain(nil, failingStore{})
	if _, err := kc.Resolve(fakeRes("example.test")); err == nil {
		t.Fatal("store error was swallowed")
	}
	if _, err := NewStore(""); err == nil {
		t.Fatal("empty store path accepted")
	}
}

func TestStoreRejectsCorruptEncodings(t *testing.T) {
	for _, raw := range []string{
		`{"auths":{"example.test":{"auth":"%%%"}}}`,
		`{"auths":{"example.test":{"auth":"bm9jb2xvbg=="}}}`,
		`{`,
	} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get("example.test"); err == nil {
				t.Fatal("corrupt credential store accepted")
			}
		})
	}
}

type failingStore struct{}

func (failingStore) Get(string) (*Credentials, error)            { return nil, errors.New("boom") }
func (failingStore) GetFor(string, string) (*Credentials, error) { return nil, errors.New("boom") }
func (failingStore) Accounts() ([]Account, error)                { return nil, errors.New("boom") }
func (failingStore) Put(Credentials) error                       { return errors.New("boom") }
func (failingStore) Delete(string) error                         { return errors.New("boom") }
func (failingStore) DeleteFor(string, string) (bool, error)      { return false, errors.New("boom") }
func (failingStore) List() ([]string, error)                     { return nil, errors.New("boom") }

var _ authn.Keychain = NewKeychain(nil, nil)
