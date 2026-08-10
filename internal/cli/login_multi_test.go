package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/manprint/backimage/pkg/registry"
)

// storeWithAccounts prepares an auth file with the given host/user pairs and
// points the CLI at it.
func storeWithAccounts(t *testing.T, pairs ...[2]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("BACKIMAGE_AUTH_FILE", path)
	s, err := registry.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pairs {
		if err := s.Put(registry.Credentials{Registry: p[0], Username: p[1], Secret: "secret-" + p[1]}); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestLoginListShowsProviderAccountAndOwner(t *testing.T) {
	path := storeWithAccounts(t,
		[2]string{"docker.io", "user1"},
		[2]string{"docker.io", "user2"},
		[2]string{"ghcr.io", "manprint"},
	)
	out, _, err := runRoot(t, "login", "--list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "PROVIDER") || !strings.Contains(out, "ACCOUNT") {
		t.Fatalf("missing header: %q", out)
	}
	for _, want := range []string{"index.docker.io", "user1", "user2", "ghcr.io", "manprint", path} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q missing %q", out, want)
		}
	}
	// Two Docker Hub rows, one per account.
	if got := strings.Count(out, "index.docker.io"); got != 2 {
		t.Fatalf("index.docker.io rows = %d, want 2", got)
	}
}

func TestLoginListJSONColumns(t *testing.T) {
	storeWithAccounts(t, [2]string{"docker.io", "user1"})
	out, _, err := runRoot(t, "--json", "login", "--list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"provider"`, `"index.docker.io"`, `"account"`, `"user1"`, `"localUser"`, `"authFile"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("JSON %q missing %q", out, want)
		}
	}
}

func TestLogoutRefusesToGuessBetweenAccounts(t *testing.T) {
	storeWithAccounts(t,
		[2]string{"docker.io", "user1"},
		[2]string{"docker.io", "user2"},
		[2]string{"docker.io", "user3"},
	)
	_, _, err := runRoot(t, "logout", "docker.io")
	if err == nil {
		t.Fatal("logout removed something without being told which account")
	}
	for _, want := range []string{"user1", "user2", "user3", "--user", "--all"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}

	// Named removal takes only that account.
	if _, _, err := runRoot(t, "logout", "docker.io", "--user", "user2"); err != nil {
		t.Fatal(err)
	}
	store, err := registry.NewStore(authFilePath())
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := store.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts after --user logout = %+v", accounts)
	}

	// --all clears the host.
	if _, _, err := runRoot(t, "logout", "docker.io", "--all"); err != nil {
		t.Fatal(err)
	}
	if accounts, _ := store.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts after --all = %+v", accounts)
	}
}

func TestLogoutSingleAccountNeedsNoSelector(t *testing.T) {
	storeWithAccounts(t, [2]string{"ghcr.io", "me"})
	if _, _, err := runRoot(t, "logout", "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	store, _ := registry.NewStore(authFilePath())
	if accounts, _ := store.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestLogoutRejectsConflictingSelectors(t *testing.T) {
	storeWithAccounts(t, [2]string{"docker.io", "user1"}, [2]string{"docker.io", "user2"})
	_, _, err := runRoot(t, "logout", "docker.io", "--all", "--user", "user1")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v", err)
	}
}

// --registry-user is a root flag, so every subcommand must see it; the check
// is offline because the value is read before any network call.
func TestRegistryUserFlagIsVisibleToSubcommands(t *testing.T) {
	root := NewRootCommand()
	if err := root.PersistentFlags().Set("registry-user", "user2"); err != nil {
		t.Fatal(err)
	}
	sub, _, err := root.Find([]string{"repo", "tags"})
	if err != nil {
		t.Fatal(err)
	}
	if got := registryUser(sub); got != "user2" {
		t.Fatalf("registryUser from %s = %q", sub.Name(), got)
	}
	opts, err := parseOptions(root)
	if err != nil {
		t.Fatal(err)
	}
	if opts.RegistryUser != "user2" {
		t.Fatalf("Options.RegistryUser = %q", opts.RegistryUser)
	}
	if got := registryUser(NewRootCommand()); got != "" {
		t.Fatalf("default registryUser = %q, want empty", got)
	}
}
