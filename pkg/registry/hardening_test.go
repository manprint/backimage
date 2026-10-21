package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An https registry that named an http realm got the Basic credentials sent
// to it in clear text.
func TestRealmCannotDowngradeTheScheme(t *testing.T) {
	secure, _ := url.Parse("https://registry.test/v2/")
	local, _ := url.Parse("http://127.0.0.1:5000/v2/")
	for _, tc := range []struct {
		realm string
		base  *url.URL
		want  string
		bad   bool
	}{
		{"https://auth.test/token", secure, "https://auth.test/token", false},
		{"auth.test/token", secure, "https://auth.test/token", false},
		{"", secure, "", false},
		{"http://127.0.0.1:5000/token", local, "http://127.0.0.1:5000/token", false},
		{"http://auth.test/token", secure, "", true},
		{"HTTP://auth.test/token", secure, "", true},
		{"ftp://auth.test/token", secure, "", true},
	} {
		got, err := resolveRealm(tc.realm, tc.base)
		if tc.bad != (err != nil) || (!tc.bad && got != tc.want) {
			t.Errorf("resolveRealm(%q, %s) = %q, %v", tc.realm, tc.base, got, err)
		}
	}
}

// The parameters were split on every comma, quoted values included, so a
// scope naming two actions lost the second.
func TestBearerChallengeKeepsQuotedValuesWhole(t *testing.T) {
	realm, params, err := parseBearerChallenge(`Bearer Realm="https://auth.example/token", service="hub",scope="repository:x:pull,push",error="a \"quoted\" word",bare=token`)
	if err != nil {
		t.Fatal(err)
	}
	if realm != "https://auth.example/token" {
		t.Errorf("realm = %q", realm)
	}
	for key, want := range map[string]string{
		"service": "hub", "scope": "repository:x:pull,push", "error": `a "quoted" word`, "bare": "token",
	} {
		if got := params.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, _, err := parseBearerChallenge(`Bearer realm="https://auth.example/token`); err == nil {
		t.Error("an unterminated quoted value was accepted")
	}
}

// tokenServer answers the /v2/ probe with a bearer challenge and the token
// endpoint with body.
func tokenServer(t *testing.T, body func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token",service="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The token answer was decoded without a bound: an issuer that streams
// without end held the client and its memory.
func TestTokenAnswerIsBounded(t *testing.T) {
	huge := func(w http.ResponseWriter) {
		fmt.Fprint(w, `{"token":"`)
		w.Write(bytes.Repeat([]byte("a"), 2<<20))
		fmt.Fprint(w, `"}`)
	}
	srv := tokenServer(t, huge)
	if tok, err := NewProvider(srv.URL, nil).Get(context.Background(), Scope{Repository: "r"}); err == nil {
		t.Fatalf("a %d-byte token answer was accepted", len(tok.Value))
	}
	if err := VerifyCredentials(context.Background(), srv.URL, Credentials{Username: "u", Secret: "p"}); err == nil {
		t.Fatal("credentials were accepted on an oversized token answer")
	}
	// An ordinary answer still works.
	ok := tokenServer(t, func(w http.ResponseWriter) { fmt.Fprint(w, `{"token":"t","expires_in":60}`) })
	if _, err := NewProvider(ok.URL, nil).Get(context.Background(), Scope{Repository: "r"}); err != nil {
		t.Fatal(err)
	}
}

// expires_in times a second overflowed: 2^62 became a zero lifetime, a token
// already expired when minted.
func TestTokenLifetimeDoesNotOverflow(t *testing.T) {
	srv := tokenServer(t, func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"token":"t","expires_in":%d}`, int64(1)<<62)
	})
	tok, err := NewProvider(srv.URL, nil).Get(context.Background(), Scope{Repository: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !tok.ExpiresAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("token minted with expires_in=2^62 expires at %v", tok.ExpiresAt)
	}
}

// The read-back read the whole manifest answer before looking at it: a
// registry streaming a huge body filled the client's memory.
func TestManifestReadBackIsBounded(t *testing.T) {
	fr := newFakeRegistry()
	fr.floodManifest = 128 << 20
	srv := fr.server()
	defer srv.Close()

	err := pushOne(t, srv.URL, testImage(t, bytes.Repeat([]byte("f"), 32)), PushOptions{Jobs: 1})
	if !errors.Is(err, ErrPublishedMismatch) {
		t.Fatalf("an oversized manifest answer = %v, want ErrPublishedMismatch", err)
	}
	if sent := atomic.LoadInt64(&fr.floodSent); sent >= 64<<20 {
		t.Fatalf("the client read %d bytes of a manifest it pushed with a few hundred", sent)
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("error must name the manifest: %v", err)
	}
}
