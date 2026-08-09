package registry

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// bearerAuth attaches the current bearer token to a request and performs a
// single retry after invalidating an expired token on 401.
type bearerAuth struct {
	base  http.RoundTripper
	p     Provider
	scope Scope
}

// NewRoundTripper returns an http.RoundTripper that attaches a bearer token
// to every request and retries exactly once after invalidating the token
// when the server answers 401. Requests with a body and no GetBody are not
// retried (the body could not be replayed); requests without a body are
// always retried with a fresh token.
func NewRoundTripper(base http.RoundTripper, p Provider, scope Scope) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &bearerAuth{base: base, p: p, scope: scope}
}

func (b *bearerAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return b.base.RoundTrip(req)
	}
	token, err := b.p.Get(req.Context(), b.scope)
	if err != nil {
		return nil, fmt.Errorf("acquiring token: %w", err)
	}
	attached := b.withToken(req, token)
	resp, err := b.base.RoundTrip(attached)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// One retry with a fresh token, only if the request is replayable.
	if req.Body != nil && req.GetBody == nil {
		return resp, nil // let the caller surface the 401
	}
	if _, copyErr := io.Copy(io.Discard, resp.Body); copyErr != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("draining 401 response: %w", copyErr)
	}
	_ = resp.Body.Close()
	b.p.Invalidate(b.scope)
	token, err = b.p.Get(req.Context(), b.scope)
	if err != nil {
		return nil, fmt.Errorf("refreshing token after 401: %w", err)
	}
	retry := b.withToken(req, token)
	if req.Body != nil {
		retry.Body, err = req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("replaying request body after 401: %w", err)
		}
	}
	second, err := b.base.RoundTrip(retry)
	if err != nil {
		return nil, err
	}
	return second, nil
}

func (b *bearerAuth) withToken(req *http.Request, token *Token) *http.Request {
	cloned := req.Clone(req.Context())
	if token == nil || (token.Value == "" && token.authorization == "") {
		cloned.Header.Del("Authorization")
	} else if token.authorization != "" {
		cloned.Header.Set("Authorization", token.authorization)
	} else {
		cloned.Header.Set("Authorization", "Bearer "+token.Value)
	}
	return cloned
}

// RoundTripWithRetry is used when the caller already knows the token.
func (b *bearerAuth) RefreshToken(req *http.Request) (*http.Response, error) {
	return b.RoundTrip(req)
}

var _ context.Context = context.Background()
