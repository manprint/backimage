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
	client := newRegistryClient(NewProvider(ref.Context().RegistryStr(), auth), scope)
	client.Timeout = 30 * time.Second
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
	realm, err = resolveRealm(realm, first.Request.URL)
	if err != nil {
		return fmt.Errorf("registry %s: %w", host, err)
	}
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
	data, err := readBounded(resp.Body, maxTokenAnswer)
	if err != nil {
		return "", fmt.Errorf("token body: %w", err)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", fmt.Errorf("token body: %w", err)
	}
	if body.Token == "" {
		return "", fmt.Errorf("empty token")
	}
	return body.Token, nil
}

// resolveRealm absolutizes a realm returned by WWW-Authenticate when the
// registry omitted the scheme (allowed per RFC 6750; tests do it). It refuses
// a realm reached over a weaker scheme than the registry itself: the token
// request carries the Basic credentials, and an https registry naming an
// http realm would have them sent in clear text.
func resolveRealm(realm string, base *url.URL) (string, error) {
	if realm == "" {
		return "", nil
	}
	if !strings.Contains(realm, "://") {
		// realm is a bare authority + path: absolutize with the request scheme.
		realm = base.Scheme + "://" + realm
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("token realm: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if base.Scheme != "http" {
			return "", fmt.Errorf("token realm %s is plain http for an %s registry", u.Redacted(), base.Scheme)
		}
	default:
		return "", fmt.Errorf("token realm %s: unsupported scheme %q", u.Redacted(), u.Scheme)
	}
	return realm, nil
}

// parseBearerChallenge extracts realm/service/scope from a
// WWW-Authenticate: Bearer ... header. Parameter names are case-insensitive
// and a quoted value is taken whole (RFC 7235): a scope may itself hold
// commas, as in "repository:x:pull,push".
func parseBearerChallenge(h string) (string, url.Values, error) {
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return "", nil, fmt.Errorf("unsupported challenge %q", h)
	}
	params := url.Values{}
	rest := h[7:]
	for {
		rest = strings.TrimLeft(rest, " \t,")
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = strings.TrimLeft(rest[eq+1:], " \t")
		var value string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			closed := false
			i := 1
			for ; i < len(rest); i++ {
				c := rest[i]
				if c == '\\' && i+1 < len(rest) {
					i++
					b.WriteByte(rest[i])
					continue
				}
				if c == '"' {
					closed = true
					break
				}
				b.WriteByte(c)
			}
			if !closed {
				return "", nil, fmt.Errorf("challenge parameter %q: unterminated quoted value", key)
			}
			value, rest = b.String(), rest[i+1:]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			value, rest = strings.TrimSpace(rest[:end]), rest[end:]
		}
		params.Set(key, value)
	}
	return params.Get("realm"), params, nil
}

// maxTokenAnswer bounds the answer of a token endpoint: a bearer token is a
// few KiB, the largest issued in practice a few tens.
const maxTokenAnswer = 1 << 20

// readBounded reads r to the end and fails, instead of truncating, once it
// holds more than limit bytes: a peer that streams without end must not
// exhaust the memory of the client.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("answer exceeds %d bytes", limit)
	}
	return data, nil
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
