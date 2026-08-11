package registry

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Three Docker Hub accounts must coexist: logging in as user2 must not
// replace user1, which is what the single-key layout used to do.
func TestStoreKeepsSeveralAccountsPerHost(t *testing.T) {
	s := newTestStore(t)
	for _, u := range []string{"user1", "user2", "user3"} {
		if err := s.Put(Credentials{Registry: "docker.io", Username: u, Secret: "s-" + u}); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := s.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 {
		t.Fatalf("accounts = %+v", accounts)
	}
	for _, u := range []string{"user1", "user2", "user3"} {
		c, err := s.GetFor("index.docker.io", u)
		if err != nil || c == nil || c.Secret != "s-"+u {
			t.Fatalf("GetFor(%q) = %+v, %v", u, c, err)
		}
	}
	// Hosts are still listed once.
	hosts, err := s.List()
	if err != nil || len(hosts) != 1 || hosts[0] != "index.docker.io" {
		t.Fatalf("List = %v, %v", hosts, err)
	}
	// Get cannot choose for the caller any more.
	if _, err := s.Get("docker.io"); !errors.Is(err, ErrAmbiguousAccount) {
		t.Fatalf("Get error = %v, want ErrAmbiguousAccount", err)
	}

	// Re-login as an existing user overwrites only that account.
	if err := s.Put(Credentials{Registry: "docker.io", Username: "user2", Secret: "new"}); err != nil {
		t.Fatal(err)
	}
	if accounts, _ := s.Accounts(); len(accounts) != 3 {
		t.Fatalf("re-login changed the account count: %+v", accounts)
	}
	if c, _ := s.GetFor("docker.io", "user2"); c == nil || c.Secret != "new" {
		t.Fatalf("re-login did not update the secret: %+v", c)
	}
}

func TestStoreKeepsTokenAndNamedAccountInEitherOrder(t *testing.T) {
	for _, tokenFirst := range []bool{false, true} {
		name := "named-first"
		if tokenFirst {
			name = "token-first"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			named := Credentials{Registry: "ghcr.io", Username: "alice", Secret: "password"}
			token := TokenCredentials("ghcr.io", "bearer")
			if tokenFirst {
				if err := s.Put(token); err != nil {
					t.Fatal(err)
				}
				if err := s.Put(named); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.Put(named); err != nil {
					t.Fatal(err)
				}
				if err := s.Put(token); err != nil {
					t.Fatal(err)
				}
			}

			if accounts, err := s.Accounts(); err != nil || len(accounts) != 2 {
				t.Fatalf("accounts = %+v, %v", accounts, err)
			}
			if got, err := s.GetFor("ghcr.io", "alice"); err != nil || got == nil || got.Secret != "password" {
				t.Fatalf("named account = %+v, %v", got, err)
			}
			got, err := s.GetFor("ghcr.io", TokenAccountName)
			if err != nil || got == nil || got.Secret != "bearer" {
				t.Fatalf("token account = %+v, %v", got, err)
			}
			auth, err := NewKeychainForUser(nil, s, TokenAccountName).Resolve(fakeRepo("ghcr.io", "team/image"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg, _ := auth.Authorization(); cfg.RegistryToken != "bearer" {
				t.Fatalf("selected token = %+v", cfg)
			}
			if removed, err := s.DeleteFor("ghcr.io", TokenAccountName); err != nil || !removed {
				t.Fatalf("DeleteFor token = %v, %v", removed, err)
			}
			if got, _ := s.GetFor("ghcr.io", "alice"); got == nil {
				t.Fatal("deleting token removed named account")
			}
		})
	}
}

func TestStoreDeleteForAndDeleteAll(t *testing.T) {
	s := newTestStore(t)
	for _, u := range []string{"user1", "user2"} {
		if err := s.Put(Credentials{Registry: "docker.io", Username: u, Secret: u}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(Credentials{Registry: "ghcr.io", Username: "me", Secret: "x"}); err != nil {
		t.Fatal(err)
	}
	removed, err := s.DeleteFor("docker.io", "user1")
	if err != nil || !removed {
		t.Fatalf("DeleteFor = %v, %v", removed, err)
	}
	if c, _ := s.GetFor("docker.io", "user1"); c != nil {
		t.Fatal("user1 survived DeleteFor")
	}
	if c, _ := s.GetFor("docker.io", "user2"); c == nil {
		t.Fatal("DeleteFor removed the wrong account")
	}
	if removed, _ := s.DeleteFor("docker.io", "absent"); removed {
		t.Fatal("DeleteFor reported a removal that did not happen")
	}
	if err := s.Delete("docker.io"); err != nil {
		t.Fatal(err)
	}
	accounts, _ := s.Accounts()
	if len(accounts) != 1 || accounts[0].Registry != "ghcr.io" {
		t.Fatalf("Delete touched another host: %+v", accounts)
	}
}

// A file written by an older version keys the entry by host only; it must keep
// working and must not be duplicated when the same user logs in again.
func TestStoreReadsLegacyLayout(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Credentials{Registry: "docker.io", Username: "user1", Secret: "old"}); err != nil {
		t.Fatal(err)
	}
	accounts, _ := s.Accounts()
	if len(accounts) != 1 || accounts[0].Username != "user1" {
		t.Fatalf("legacy entry not read: %+v", accounts)
	}
	if err := s.Put(Credentials{Registry: "docker.io", Username: "user1", Secret: "fresh"}); err != nil {
		t.Fatal(err)
	}
	accounts, _ = s.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("re-login duplicated the legacy entry: %+v", accounts)
	}
	if c, _ := s.GetFor("docker.io", "user1"); c == nil || c.Secret != "fresh" {
		t.Fatalf("legacy entry not updated: %+v", c)
	}
}

func TestKeychainSelectsAccountByNamespace(t *testing.T) {
	s := newTestStore(t)
	for _, u := range []string{"user1", "user2", "user3"} {
		if err := s.Put(Credentials{Registry: "docker.io", Username: u, Secret: "s-" + u}); err != nil {
			t.Fatal(err)
		}
	}
	kc := NewKeychain(nil, s)

	auth, err := kc.Resolve(fakeRepo("index.docker.io", "user2/myimage"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := auth.Authorization()
	if cfg.Username != "user2" || cfg.Password != "s-user2" {
		t.Fatalf("namespace selection failed: %+v", cfg)
	}

	// Unknown namespace: stop and name the candidates.
	_, err = kc.Resolve(fakeRepo("index.docker.io", "someoneelse/img"))
	if err == nil {
		t.Fatal("unmatched namespace resolved silently")
	}
	for _, want := range []string{"user1", "user2", "user3", "--registry-user"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	// Explicit selection wins over the namespace.
	kc = NewKeychainForUser(nil, s, "user3")
	auth, err = kc.Resolve(fakeRepo("index.docker.io", "user1/img"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ = auth.Authorization(); cfg.Username != "user3" {
		t.Fatalf("--registry-user ignored: %+v", cfg)
	}

	// An unknown selector is an error, not a silent fallback.
	kc = NewKeychainForUser(nil, s, "nobody")
	if _, err := kc.Resolve(fakeRepo("index.docker.io", "user1/img")); err == nil {
		t.Fatal("unknown --registry-user accepted")
	}

	// The anonymous selector bypasses the stored logins entirely.
	kc = NewKeychainForUser(nil, s, AnonymousUser)
	auth, err = kc.Resolve(fakeRepo("index.docker.io", "library/alpine"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ = auth.Authorization(); cfg.Username != "" || cfg.Password != "" {
		t.Fatalf("anonymous selector still sent credentials: %+v", cfg)
	}
}

// A host-wide token has no username to match, so it is used when it is the
// only login for that host.
func TestKeychainUsesLoneHostToken(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(TokenCredentials("ghcr.io", "tok")); err != nil {
		t.Fatal(err)
	}
	accounts, _ := s.Accounts()
	if len(accounts) != 1 || !accounts[0].Token || accounts[0].Username != "" {
		t.Fatalf("token account = %+v", accounts)
	}
	auth, err := NewKeychain(nil, s).Resolve(fakeRepo("ghcr.io", "team/dumps"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := auth.Authorization()
	if cfg.RegistryToken != "tok" {
		t.Fatalf("token not used: %+v", cfg)
	}
}
