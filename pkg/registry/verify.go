package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// VerifyPushAccess resolves credentials and requests repository push scope
// before the backup performs expensive archive/compression work. A HEAD for
// an impossible blob proves that authentication and repository routing work;
// 404 is the expected successful result.
func VerifyPushAccess(ctx context.Context, ref name.Reference, kc Keychain) error {
	auth := authn.Anonymous
	if kc != nil {
		resolved, err := kc.Resolve(ref.Context())
		if err != nil {
			return fmt.Errorf("resolving credentials: %w", err)
		}
		auth = resolved
	}
	base, err := httpBase(ref.Context().RegistryStr())
	if err != nil {
		return err
	}
	scope := Scope{Repository: ref.Context().RepositoryStr(), Actions: []string{"pull", "push"}}
	client := &http.Client{
		Transport: NewRoundTripper(http.DefaultTransport, NewProvider(ref.Context().RegistryStr(), auth), scope),
		Timeout:   30 * time.Second,
	}
	probe := base + "/v2/" + ref.Context().RepositoryStr() + "/blobs/sha256:" + strings.Repeat("0", 64)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, probe, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("registry push preflight: %w", err)
	}
	defer resp.Body.Close()
	if _, copyErr := io.Copy(io.Discard, resp.Body); copyErr != nil {
		return fmt.Errorf("registry push preflight response: %w", copyErr)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNotFound:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("registry push preflight: credentials rejected by %s (%d)", ref.Context().RegistryStr(), resp.StatusCode)
	default:
		return fmt.Errorf("registry push preflight: %s answered %d", ref.Context().RegistryStr(), resp.StatusCode)
	}
}

// VerifyCredentials checks that c can authenticate against the registry
// host. It performs a credentialed GET /v2/; when the registry challenges
// with bearer auth (Docker-style), it requests a token for the scope
// registry:catalog:* — if the registry refuses a token it retries with an
// empty scope, so the /v2/ answer decides.
//
// A nil error means the credentials were accepted. Errors never contain
// the secret.
func VerifyCredentials(ctx context.Context, host string, c Credentials) error {
	base, err := httpBase(host)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}

	first, err := credentialedRequest(ctx, client, base+"/v2/", c)
	if err != nil {
		return fmt.Errorf("registry %s: %w", host, err)
	}
	code := first.StatusCode
	challenge := first.Header.Get("WWW-Authenticate")
	first.Body.Close()

	if code == http.StatusOK || code == http.StatusAccepted {
		return nil
	}
	if c.Username == tokenUsername {
		return fmt.Errorf("token rejected: %s answered %d", host, code)
	}
	if challenge == "" {
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			return fmt.Errorf("credentials rejected: %s answered %d", host, code)
		}
		return fmt.Errorf("registry %s: unexpected status %d", host, code)
	}
	realm, params, err := parseBearerChallenge(challenge)
	if err != nil {
		return fmt.Errorf("registry %s: %w", host, err)
	}
	realm = resolveRealm(realm, first.Request.URL)
	if realm == "" {
		return fmt.Errorf("credentials rejected: %s answered %d", host, code)
	}

	for _, scope := range []string{"registry:catalog:*", ""} {
		_, err := exchangeToken(ctx, client, realm, params, scope, c)
		if err == nil {
			return nil // a token was minted: credentials are accepted
		}
	}
	return fmt.Errorf("credentials rejected: %s answered %d", host, code)
}

func credentialedRequest(ctx context.Context, client *http.Client, u string, c Credentials) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.Username == tokenUsername {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	} else {
		req.Header.Set("Authorization", basicAuthHeader(c.Username, c.Secret))
	}
	req.Header.Set("User-Agent", "backimage")
	return client.Do(req)
}

// exchangeToken performs the classic bearer token exchange: GET realm with
// service/scope and Basic credentials; a 200 with a token proves the
// credentials are valid.
func exchangeToken(ctx context.Context, client *http.Client, realm string, params url.Values, scope string, c Credentials) (string, error) {
	q := url.Values{}
	if v := params.Get("service"); v != "" {
		q.Set("service", v)
	}
	if scope != "" {
		q.Set("scope", scope)
	} else if v := params.Get("scope"); v != "" {
		q.Set("scope", v)
	}
	sep := "?"
	if strings.Contains(realm, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+sep+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", basicAuthHeader(c.Username, c.Secret))
	req.Header.Set("User-Agent", "backimage")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token status %d", resp.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("token body: %w", err)
	}
	if body.Token == "" {
		return "", fmt.Errorf("empty token")
	}
	return body.Token, nil
}

// parseBearerChallenge extracts realm/service/scope from a
// WWW-Authenticate: Bearer ... header.
// resolveRealm absolutizes a realm returned by WWW-Authenticate when the
// registry omitted the scheme (allowed per RFC 6750; tests do it).
func resolveRealm(realm string, base *url.URL) string {
	if realm == "" || strings.Contains(realm, "://") {
		return realm
	}
	// realm is a bare authority + path: absolutize with the request scheme.
	return base.Scheme + "://" + realm
}

func parseBearerChallenge(h string) (string, url.Values, error) {
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return "", nil, fmt.Errorf("unsupported challenge %q", h)
	}
	params := url.Values{}
	for _, kv := range strings.Split(h[7:], ",") {
		parts := strings.SplitN(strings.TrimSpace(kv), "=", 2)
		if len(parts) != 2 {
			continue
		}
		params.Set(parts[0], strings.Trim(parts[1], `"`))
	}
	return params.Get("realm"), params, nil
}

func basicAuthHeader(user, secret string) string {
	raw := base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
	return "Basic " + raw
}

// httpBase normalizes host into an https:// base URL (http allowed for
// loopback, useful for in-process tests and local registries).
func httpBase(host string) (string, error) {
	h := strings.TrimSpace(host)
	if !strings.Contains(h, "://") {
		u, err := url.Parse("//" + h)
		if err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") {
			h = "http://" + h
		} else {
			h = "https://" + h
		}
	}
	u, err := url.Parse(h)
	if err != nil {
		return "", fmt.Errorf("invalid registry host %q: %w", host, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("invalid registry scheme %q", u.Scheme)
	}
	return strings.TrimRight(h, "/"), nil
}
