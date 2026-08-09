package registry

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
)

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Credentials{Registry: "ghcr.io", Username: "me", Secret: "sup3r"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Get("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "me" || c.Secret != "sup3r" {
		t.Fatalf("got %+v", c)
	}
	if got, err := s.Get("missing.example"); err != nil || got != nil {
		t.Fatalf("missing: %v %v", got, err)
	}
	if err := s.Delete("ghcr.io"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Get("ghcr.io"); c != nil {
		t.Fatal("delete failed")
	}
	if err := s.Delete("ghcr.io"); err != nil {
		t.Fatal("second delete must be a no-op")
	}
}

func TestStoreListNeverSecrets(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range []string{"ghcr.io", "quay.io"} {
		if err := s.Put(Credentials{Registry: reg, Username: "u", Secret: "s3cr3t"}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 || strings.Join(l, ",") != "ghcr.io,quay.io" {
		t.Fatalf("list = %v", l)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatal("secret leaked into the file")
	}
}

func TestStoreInsecurePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms ignored on windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(p)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("want chmod-600 hint, got %v", err)
	}
}

func TestStoreAtomicWriteNoCorruption(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := s.Put(Credentials{Registry: "r.io", Username: "u", Secret: "x"}); err == nil {
		t.Fatal("want error on unwritable dir")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("failed write must not leave a file")
	}
}

func TestCanonicalizeDockerHubAliases(t *testing.T) {
	for _, in := range []string{"docker.io", "index.docker.io", "registry-1.docker.io"} {
		if got := CanonicalHost(in); got != "index.docker.io" {
			t.Errorf("%s -> %s", in, got)
		}
	}
	for _, in := range []string{"ghcr.io", "localhost:5000", "https://quay.io/"} {
		if got := CanonicalHost(in); got != strings.TrimSuffix(strings.ToLower(in), "/") && !strings.HasPrefix(got, "quay.io") {
			t.Errorf("unexpected canonicalization of %s -> %s", in, got)
		}
	}
}

func TestStoreAliasEqual(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Credentials{Registry: "docker.io", Username: "x", Secret: "y"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Get("registry-1.docker.io")
	if err != nil || c == nil || c.Secret != "y" {
		t.Fatalf("alias lookup failed: %v %v", c, err)
	}
	l, _ := s.List()
	if len(l) != 1 || l[0] != "index.docker.io" {
		t.Fatalf("list = %v", l)
	}
}

func TestStoreDockerCompatField(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Credentials{Registry: "ghcr.io", Username: "user", Secret: "pass"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	if !strings.Contains(string(raw), `"auths"`) || !strings.Contains(string(raw), `"ghcr.io"`) {
		t.Fatalf("not docker-compatible: %s", raw)
	}
}

func fakeRes(reg string) authn.Resource {
	return &fakeReg{reg: reg}
}

type fakeReg struct{ reg string }

func (f *fakeReg) RegistryStr() string   { return f.reg }
func (f *fakeReg) RepositoryStr() string { return "repo" }
func (f *fakeReg) String() string        { return f.reg + "/repo" }

func TestKeychainLayers(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Credentials{Registry: "ghcr.io", Username: "me", Secret: "tok"}); err != nil {
		t.Fatal(err)
	}
	// explicit wins over store
	kc := NewKeychain(&Credentials{Registry: "ghcr.io", Username: "ex", Secret: "pl"}, s)
	a, err := kc.Resolve(fakeRes("ghcr.io"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "ex" || cfg.Password != "pl" {
		t.Fatalf("explicit not used: %+v", cfg)
	}
	// store is used for unknown explicit
	kc = NewKeychain(nil, s)
	a, err = kc.Resolve(fakeRes("ghcr.io"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ = a.Authorization()
	if cfg.Username != "me" || cfg.Password != "tok" {
		t.Fatalf("store not used: %+v", cfg)
	}
	// unknown registry degrades to anonymous but never fails
	_, err = kc.Resolve(fakeRes("example.invalid"))
	if err != nil {
		t.Fatal(err)
	}
}
