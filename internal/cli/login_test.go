package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loginTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BACKIMAGE_AUTH_FILE", filepath.Join(dir, "auth.json"))
	return dir
}

func authServer(t *testing.T, user, pass string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if ok && u == user && p == pass {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token",service="t"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/v2/token", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"token":"ok","expires_in":60}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginPasswordStdinOK(t *testing.T) {
	srv := authServer(t, "me", "pw")
	loginTestEnv(t)
	root := NewRootCommand()
	root.SetArgs([]string{"login", srv.URL, "-u", "me", "--password-stdin"})
	root.SetIn(strings.NewReader("pw\n"))
	var out strings.Builder
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(out.String(), "login succeeded") {
		t.Fatalf("output: %q", out.String())
	}
	// persisted and listable, secret hidden
	root = NewRootCommand()
	root.SetArgs([]string{"login", "--list"})
	var out2 strings.Builder
	root.SetOut(&out2)
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out2.String(), "127.0.0.1") || strings.Contains(out2.String(), "pw") {
		t.Fatalf("list leaked: %q", out2.String())
	}
}

func TestLoginWrongCredsNotSaved(t *testing.T) {
	srv := authServer(t, "me", "pw")
	path := loginTestEnv(t)
	root := NewRootCommand()
	root.SetArgs([]string{"login", srv.URL, "-u", "me", "--password-stdin"})
	root.SetIn(strings.NewReader("WRONG\n"))
	var errOut strings.Builder
	root.SetErr(&errOut)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected failure")
	}
	if ExitCodeFor(err) != 6 {
		t.Fatalf("want exit 6, got %d", ExitCodeFor(err))
	}
	if _, statErr := os.Stat(filepath.Join(path, "auth.json")); statErr == nil {
		t.Fatal("auth.json must not be written on failed login")
	}
}

func TestLoginPasswordWarning(t *testing.T) {
	srv := authServer(t, "me", "pw")
	loginTestEnv(t)
	root := NewRootCommand()
	root.SetArgs([]string{"login", srv.URL, "-u", "me", "-p", "pw"})
	var errOut strings.Builder
	root.SetErr(&errOut)
	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(errOut.String(), "visible in the process list") {
		t.Fatalf("missing warning: %q", errOut.String())
	}
}

func TestLoginPasswordStdinConflict(t *testing.T) {
	loginTestEnv(t)
	root := NewRootCommand()
	root.SetArgs([]string{"login", "-u", "me", "-p", "x", "--password-stdin"})
	if err := root.Execute(); ExitCodeFor(err) != 2 {
		t.Fatalf("want usage error, got %v (code %d)", err, ExitCodeFor(err))
	}
}

func TestLoginListJSON(t *testing.T) {
	srv := authServer(t, "me", "pw")
	loginTestEnv(t)
	root := NewRootCommand()
	root.SetArgs([]string{"login", srv.URL, "-u", "me", "--password-stdin"})
	root.SetIn(strings.NewReader("pw\n"))
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	root = NewRootCommand()
	root.SetArgs([]string{"login", "--list", "--json"})
	var out strings.Builder
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	trim := strings.TrimSpace(out.String())
	if !strings.HasPrefix(trim, "[") || !strings.HasSuffix(trim, "]") || strings.Contains(trim, "pw") {
		t.Fatalf("json list: %q", trim)
	}
}

func TestLogout(t *testing.T) {
	srv := authServer(t, "me", "pw")
	loginTestEnv(t)
	root := NewRootCommand()
	root.SetArgs([]string{"login", srv.URL, "-u", "me", "--password-stdin"})
	root.SetIn(strings.NewReader("pw\n"))
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	root = NewRootCommand()
	root.SetArgs([]string{"logout", srv.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("logout: %v", err)
	}
	root = NewRootCommand()
	root.SetArgs([]string{"login", "--list"})
	var out strings.Builder
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "127.0.0.1") {
		t.Fatalf("still logged in: %q", out.String())
	}
}

func TestLoginNoCredsNoTTY(t *testing.T) {
	loginTestEnv(t)
	root := NewRootCommand()
	root.SetArgs([]string{"login"})
	if err := root.Execute(); ExitCodeFor(err) != 2 {
		t.Fatalf("want usage error, got %v", err)
	}
}
