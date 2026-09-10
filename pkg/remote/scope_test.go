package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/registry"
	"github.com/manprint/backimage/pkg/server"
)

// hostileSink is a server that asks for credentials the backup does not need.
type hostileSink struct {
	*testSink
	repository string
	actions    []string
	tokens     atomic.Int32
}

func (s *hostileSink) TokenScope(string) (string, []string, error) {
	return s.repository, append([]string(nil), s.actions...), nil
}

func (s *hostileSink) ProvideToken(token *protocol.Token) {
	if token != nil {
		s.tokens.Add(1)
	}
}

// countingProvider fails the test by being called at all.
type countingProvider struct{ gets atomic.Int32 }

func (p *countingProvider) Get(_ context.Context, scope registry.Scope) (*registry.Token, error) {
	p.gets.Add(1)
	return &registry.Token{Value: "granted", ExpiresAt: time.Now().Add(time.Hour), Scope: scope}, nil
}
func (p *countingProvider) Invalidate(registry.Scope) {}

// TestClientRefusesAScopeItDidNotChoose is the acceptance test of A4.1: an
// authenticated server that asks for something the backup does not need must
// produce zero calls to the credential provider and zero tokens on the wire.
//
// Counting the provider calls is the point. Checking only that the upload
// failed would pass even if the token had been minted and sent first, which
// is exactly what used to happen: repository and actions came off the wire
// and went straight into Provider.Get.
func TestClientRefusesAScopeItDidNotChoose(t *testing.T) {
	cases := []struct {
		name       string
		repository string
		actions    []string
		want       string
	}{
		{"another repository", "unrelated/repository", []string{"pull", "push"}, "this backup pushes to"},
		{"a deletion", "me/repo", []string{"pull", "delete"}, `the "delete" action`},
		{"a wildcard", "me/repo", []string{"*"}, `the "*" action`},
		{"a repeated action", "me/repo", []string{"pull", "pull"}, `repeated the "pull" action`},
		{"no action at all", "me/repo", []string{}, "with no action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layer := testLayer(t, []byte("payload"))
			sink := &hostileSink{testSink: newTestSink(), repository: tc.repository, actions: tc.actions}
			dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true}, sink: sink}
			provider := &countingProvider{}
			client, err := New(Config{
				Dialer: dialer, Address: "pipe", Provider: provider, Backoffs: []time.Duration{},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Upload(context.Background(), testBackup(layer))
			if err == nil {
				t.Fatal("the upload accepted a scope the client did not choose")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
			if got := provider.gets.Load(); got != 0 {
				t.Fatalf("the credential provider was called %d times for a refused scope", got)
			}
			if got := sink.tokens.Load(); got != 0 {
				t.Fatalf("%d tokens reached the server for a refused scope", got)
			}
		})
	}
}

// TestScopeGuardAcceptsWhatTheBackupNeeds is the other half: the legitimate
// request must pass, normalised to the repository of the local reference.
func TestScopeGuardAcceptsWhatTheBackupNeeds(t *testing.T) {
	guard, err := newScopeGuard("registry.test/me/repo:t")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := guard.check(&protocol.TokenRequest{Repository: "me/repo", Actions: []string{"pull", "push"}})
	if err != nil {
		t.Fatal(err)
	}
	if scope.Repository != "me/repo" || strings.Join(scope.Actions, ",") != "pull,push" {
		t.Fatalf("scope = %+v", scope)
	}
	// Asking again for the same scope is a refresh, not a new scope.
	if _, err := guard.check(&protocol.TokenRequest{Repository: "me/repo", Actions: []string{"pull", "push"}}); err != nil {
		t.Fatalf("a repeated request for the same scope was refused: %v", err)
	}
	if _, err := guard.check(nil); err == nil {
		t.Fatal("an empty token request was accepted")
	}
	if _, err := newScopeGuard("not a reference::"); err == nil {
		t.Fatal("an unparseable reference produced a guard")
	}
}

// TestScopeGuardCapsTheScopesOfOneSession covers the defence in depth: even
// requests that pass every other rule cannot be repeated with new shapes
// forever, because each new scope starts a refresh goroutine.
func TestScopeGuardCapsTheScopesOfOneSession(t *testing.T) {
	guard, err := newScopeGuard("registry.test/me/repo:t")
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxSessionScopes {
		guard.seen[string(rune('a'+i))] = true
	}
	if _, err := guard.check(&protocol.TokenRequest{Repository: "me/repo", Actions: []string{"pull"}}); err == nil {
		t.Fatal("the session accepted more scopes than the cap allows")
	}
}

// countingRealProvider is the real registry provider, wrapped only to count
// how many times it is asked. Building the credential through it rather than
// faking one is the point: the delegability of a token is decided where the
// token is minted, and a hand-made stand-in would not exercise that.
type countingRealProvider struct {
	registry.Provider
	gets atomic.Int32
}

func (p *countingRealProvider) Get(ctx context.Context, scope registry.Scope) (*registry.Token, error) {
	p.gets.Add(1)
	return p.Provider.Get(ctx, scope)
}

// staticBearerProvider is the docker-config case: AuthConfig.RegistryToken
// set, which mint returns as it is without talking to any issuer.
func staticBearerProvider() *countingRealProvider {
	auth := authn.FromConfig(authn.AuthConfig{RegistryToken: "pat-value"})
	return &countingRealProvider{Provider: registry.NewProvider("registry.test", auth)}
}

// basicOnlyProvider is a registry that answers /v2/ with 200: it issues no
// token at all and expects HTTP Basic on every request.
func basicOnlyProvider(t *testing.T) *countingRealProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	auth := authn.FromConfig(authn.AuthConfig{Username: "user", Password: "pass"})
	return &countingRealProvider{Provider: registry.NewProvider(host, auth)}
}

// TestClientRefusesToForwardACredentialThatIsNotADelegation is the acceptance
// test of A4.2: a static bearer, and a registry that only speaks Basic, must
// stop the backup before any layer is uploaded, and the message must name the
// way out.
func TestClientRefusesToForwardACredentialThatIsNotADelegation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T) *countingRealProvider
		want  string
	}{
		{"a static bearer", func(*testing.T) *countingRealProvider { return staticBearerProvider() }, "static bearer token"},
		{"a Basic-only registry", basicOnlyProvider, "wants HTTP Basic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer := testLayer(t, []byte("payload"))
			sink := newTokenSink()
			dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true}, sink: sink}
			provider := tc.build(t)
			client, err := New(Config{Dialer: dialer, Address: "pipe", Provider: provider, Backoffs: []time.Duration{}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Upload(context.Background(), testBackup(layer)); err == nil {
				t.Fatal("a credential that is not a delegation was forwarded")
			} else {
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %q, want it to name %q", err, tc.want)
				}
				if !strings.Contains(err.Error(), "--forward-static-token") {
					t.Fatalf("error = %q, want it to name the opt-in flag", err)
				}
			}
			if got := sink.tokens.Load(); got != 0 {
				t.Fatalf("%d credentials reached the server without the opt-in", got)
			}
			if got := sink.opens.Load(); got != 0 {
				t.Fatalf("the upload started anyway: %d blobs opened", got)
			}
		})
	}
}

// TestForwardStaticTokenIsAnExplicitChoice checks the other side of the
// switch: with the opt-in the credential travels, and it travels with an
// expiry this client declares rather than one it read off the credential.
func TestForwardStaticTokenIsAnExplicitChoice(t *testing.T) {
	layer := testLayer(t, []byte("payload"))
	sink := newTokenSink()
	dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true}, sink: sink}
	provider := staticBearerProvider()
	now := time.Unix(1_700_000_000, 0)
	client, err := New(Config{
		Dialer: dialer, Address: "pipe", Provider: provider, Backoffs: []time.Duration{},
		ForwardStaticToken: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Upload(context.Background(), testBackup(layer)); err != nil {
		t.Fatalf("upload: %v (server: %v)", err, dialer.ServerError())
	}
	if got := sink.tokens.Load(); got != 1 {
		t.Fatalf("credentials delivered = %d, want exactly 1", got)
	}
	if got := sink.lastExpiry.Load(); got != now.Add(StaticTokenForwardTTL).Unix() {
		t.Fatalf("declared expiry = %d, want %d", got, now.Add(StaticTokenForwardTTL).Unix())
	}
	// A credential with no expiry to renew against must not start a refresh
	// loop that re-sends the same bytes on a timer.
	if got := provider.gets.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", got)
	}
}
