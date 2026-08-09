package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// registryHandler serves /v2/ with an optional bearer challenge and a token
// endpoint that accepts only the given username/password.
func registryHandler(user, pass string, tokenTTL int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if ok && u == user && p == pass {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token",service="test",scope="registry:catalog:*"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"good-token","expires_in":` + itoa(tokenTTL) + `}`))
	})
	return mux
}

func itoa(n int) string {
	if n <= 0 {
		return "60"
	}
	b := make([]byte, 0, 3)
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestVerifyCredentialsOK(t *testing.T) {
	srv := httptest.NewServer(registryHandler("me", "pw", 60))
	defer srv.Close()
	if err := VerifyCredentials(context.Background(), srv.URL, Credentials{Username: "me", Secret: "pw"}); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestVerifyCredentialsDirect200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if ok && u == "me" && p == "pw" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if err := VerifyCredentials(context.Background(), srv.URL, Credentials{Username: "me", Secret: "pw"}); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestVerifyCredentialsRejected(t *testing.T) {
	srv := httptest.NewServer(registryHandler("me", "pw", 60))
	defer srv.Close()
	err := VerifyCredentials(context.Background(), srv.URL, Credentials{Username: "me", Secret: "WRONG"})
	if err == nil {
		t.Fatal("expected rejection")
	}
}

func TestVerifyCredentialsNoChallenge(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if err := VerifyCredentials(context.Background(), srv.URL, Credentials{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseBearerChallenge(t *testing.T) {
	realm, params, err := parseBearerChallenge(`Bearer realm="https://auth.example/token",service="hub",scope="registry:r:p"`)
	if err != nil {
		t.Fatal(err)
	}
	if realm != "https://auth.example/token" || params.Get("service") != "hub" || params.Get("scope") != "registry:r:p" {
		t.Fatalf("realm=%s params=%v", realm, params)
	}
	if _, _, err := parseBearerChallenge(`Basic realm="x"`); err == nil {
		t.Fatal("must reject Basic challenge")
	}
}
