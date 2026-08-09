package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
)

// SafetyMargin is the proactive refresh window: tokens younger than this
// are considered stale and are renewed without waiting for a 401.
const SafetyMargin = 60 * time.Second

// DefaultTokenTTL is assumed when the registry omits expires_in.
const DefaultTokenTTL = 60 * time.Second

// Scope describes the access needed for one repository.
type Scope struct {
	Repository string   // "myuser/dumps"
	Actions    []string // "pull", "push"
}

func (s Scope) String() string {
	return "repository:" + s.Repository + ":" + strings.Join(s.Actions, ",")
}

// Token is a bearer token with its expiry.
type Token struct {
	Value         string
	ExpiresAt     time.Time
	Scope         Scope
	anonymous     bool
	authorization string
}

// Anonymous reports whether the registry accepted unauthenticated requests.
func (t *Token) Anonymous() bool { return t != nil && t.anonymous }

// NewDelegatedToken reconstructs a short-lived token received over the remote
// control stream. No credential is persisted by this operation.
func NewDelegatedToken(value string, expiresAt time.Time, scope Scope, anonymous bool) *Token {
	return &Token{Value: value, ExpiresAt: expiresAt, Scope: scope, anonymous: anonymous}
}

// Valid reports whether the token is still usable with the given margin.
func (t *Token) Valid(margin time.Duration) bool {
	if t == nil || (t.Value == "" && !t.anonymous) {
		return false
	}
	return time.Now().Add(margin).Before(t.ExpiresAt)
}

func (t *Token) validAt(now time.Time, margin time.Duration) bool {
	return t != nil && (t.Value != "" || t.anonymous) && now.Add(margin).Before(t.ExpiresAt)
}

// Provider mints and refreshes bearer tokens for one registry. It is safe
// for concurrent use and coalesces concurrent refreshes of the same scope.
type Provider interface {
	Get(ctx context.Context, scope Scope) (*Token, error)
	Invalidate(scope Scope)
}

// provider implements the Docker registry bearer-token flow, caching tokens
// per scope with coalesced in-flight refreshes.
type provider struct {
	registry string
	auth     authn.Authenticator
	client   *http.Client
	now      func() time.Time

	mu       sync.Mutex
	cache    map[string]*Token
	inflight map[string]*tokenCall
}

type tokenCall struct {
	done  chan struct{}
	token *Token
	err   error
}

var errTokenNotRequired = fmt.Errorf("registry does not require bearer authentication")

// NewProvider builds a bearer token provider for one registry. auth is
// used to answer the token endpoint's challenge (usually Basic credentials
// from the keychain).
func NewProvider(registry string, auth authn.Authenticator) Provider {
	return &provider{
		registry: registry,
		auth:     auth,
		client:   &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
		cache:    make(map[string]*Token),
		inflight: make(map[string]*tokenCall),
	}
}

// Get returns a valid token for scope, minting or refreshing as needed.
// Concurrent Get calls for the same scope share a single network request.
func (p *provider) Get(ctx context.Context, scope Scope) (*Token, error) {
	p.mu.Lock()
	key := scope.String()
	if t := p.cache[key]; t.validAt(p.now(), SafetyMargin) {
		p.mu.Unlock()
		return t, nil
	}
	if call := p.inflight[key]; call != nil {
		p.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return call.token, call.err
	}
	call := &tokenCall{done: make(chan struct{})}
	p.inflight[key] = call
	p.mu.Unlock()

	token, err := p.mint(ctx, scope)

	p.mu.Lock()
	delete(p.inflight, key)
	call.token, call.err = token, err
	if err == nil {
		p.cache[key] = token
	}
	close(call.done)
	p.mu.Unlock()
	return token, err
}

// Invalidate marks the token for scope as unusable.
func (p *provider) Invalidate(scope Scope) {
	p.mu.Lock()
	delete(p.cache, scope.String())
	p.mu.Unlock()
}

// discovery caches the realm/service of a registry between mints.
func (p *provider) realm(ctx context.Context) (string, url.Values, error) {
	base, err := httpBase(p.registry)
	if err != nil {
		return "", nil, err
	}
	baseURL, perr := url.Parse(base + "/v2/")
	if perr != nil {
		return "", nil, fmt.Errorf("registry %s: bad base url: %w", p.registry, perr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return "", nil, err
	}
	if p.auth != nil {
		cfg, authErr := p.auth.Authorization()
		if authErr != nil {
			return "", nil, fmt.Errorf("registry credentials: %w", authErr)
		}
		if cfg.Username != "" || cfg.Password != "" {
			req.Header.Set("Authorization", basicAuthHeader(cfg.Username, cfg.Password))
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("registry discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return "", nil, errTokenNotRequired
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	realm, params, err := parseBearerChallenge(challenge)
	if err != nil || realm == "" {
		return "", nil, fmt.Errorf("registry %s: unsupported authentication", p.registry)
	}
	if baseURL != nil {
		realm = resolveRealm(realm, baseURL)
	}
	return realm, params, nil
}

func (p *provider) mint(ctx context.Context, scope Scope) (*Token, error) {
	if p.auth != nil {
		cfg, authErr := p.auth.Authorization()
		if authErr != nil {
			return nil, fmt.Errorf("registry credentials: %w", authErr)
		}
		if cfg.RegistryToken != "" {
			return &Token{Value: cfg.RegistryToken, ExpiresAt: p.now().Add(24 * time.Hour), Scope: scope}, nil
		}
	}
	realm, params, err := p.realm(ctx)
	if err != nil {
		if errors.Is(err, errTokenNotRequired) {
			authorization := ""
			if p.auth != nil {
				cfg, authErr := p.auth.Authorization()
				if authErr != nil {
					return nil, fmt.Errorf("registry credentials: %w", authErr)
				}
				if cfg.Username != "" || cfg.Password != "" {
					authorization = basicAuthHeader(cfg.Username, cfg.Password)
				}
			}
			return &Token{
				ExpiresAt:     p.now().Add(24 * time.Hour),
				Scope:         scope,
				anonymous:     true,
				authorization: authorization,
			}, nil
		}
		return nil, err
	}
	q := url.Values{}
	if v := params.Get("service"); v != "" {
		q.Set("service", v)
	}
	q.Set("scope", scope.String())
	sep := "?"
	if strings.Contains(realm, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+sep+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if p.auth != nil {
		cfg, authErr := p.auth.Authorization()
		if authErr != nil {
			return nil, fmt.Errorf("registry credentials: %w", authErr)
		}
		if cfg.Username != "" || cfg.Password != "" {
			req.Header.Set("Authorization", basicAuthHeader(cfg.Username, cfg.Password))
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request (%s): %s answered %d", req.URL.String(), p.registry, resp.StatusCode)
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
		IssuedAt  string `json:"issued_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("token body: %w", err)
	}
	if body.Token == "" {
		return nil, fmt.Errorf("token body: empty token")
	}
	ttl := DefaultTokenTTL
	if body.ExpiresIn > 0 {
		ttl = time.Duration(body.ExpiresIn) * time.Second
	}
	issued := p.now()
	if body.IssuedAt != "" {
		if t, err := time.Parse(time.RFC3339, body.IssuedAt); err == nil {
			issued = t
		}
	}
	return &Token{Value: body.Token, ExpiresAt: issued.Add(ttl), Scope: scope}, nil
}

// staticProvider wraps a fixed token source (server/proxy mode, phase 08).
type staticProvider struct {
	get func(ctx context.Context, scope Scope) (*Token, error)
}

// NewStaticProvider wraps a fixed token function into a Provider.
func NewStaticProvider(get func(ctx context.Context, scope Scope) (*Token, error)) Provider {
	if get == nil {
		get = func(context.Context, Scope) (*Token, error) {
			return nil, fmt.Errorf("static token provider is not configured")
		}
	}
	return &staticProvider{get: get}
}

func (s *staticProvider) Get(ctx context.Context, scope Scope) (*Token, error) {
	return s.get(ctx, scope)
}

func (s *staticProvider) Invalidate(scope Scope) {}
