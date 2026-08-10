package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

// Credentials is a username/secret pair for one registry.
type Credentials struct {
	Registry string // as typed by the user; canonicalized on store
	Username string
	Secret   string // password or token; never logged
}

const tokenUsername = "__backimage_bearer_token__"

// TokenCredentials wraps an already-minted bearer token in the same
// Docker-compatible user:secret on-disk representation used by Store.
func TokenCredentials(registry, token string) Credentials {
	return Credentials{Registry: registry, Username: tokenUsername, Secret: token}
}

func authConfig(c Credentials) authn.AuthConfig {
	if c.Username == tokenUsername {
		return authn.AuthConfig{RegistryToken: c.Secret}
	}
	return authn.AuthConfig{Username: c.Username, Password: c.Secret}
}

// Keychain resolves credentials for a registry host.
type Keychain interface {
	// Resolve returns an authenticator for the given resource, or a
	// keyring-based authenticator when no credentials are known.
	Resolve(res authn.Resource) (authn.Authenticator, error)
}

// Account describes one stored login without exposing its secret. Several
// accounts can coexist on the same host: three Docker Hub users are three
// accounts under index.docker.io.
type Account struct {
	Registry string `json:"registry"`
	Username string `json:"username,omitempty"` // empty for a host-wide token
	Token    bool   `json:"token"`              // credential is a bearer token
}

// Store persists credentials in backimage's own file, mode 0600.
type Store interface {
	// Get returns the only account of a registry. It fails when several
	// accounts exist, because picking one silently would push with the wrong
	// identity; callers that know the wanted user call GetFor.
	Get(registry string) (*Credentials, error)
	GetFor(registry, username string) (*Credentials, error)
	// Accounts lists every stored login, never the secrets.
	Accounts() ([]Account, error)
	Put(c Credentials) error
	// Delete removes every account of a registry.
	Delete(registry string) error
	// DeleteFor removes one account; it reports whether anything was removed.
	DeleteFor(registry, username string) (bool, error)
	List() ([]string, error) // registry hosts only, never secrets
}

// ErrAmbiguousAccount reports that a registry holds several logins and the
// caller did not say which one to use.
var ErrAmbiguousAccount = errors.New("several logins for this registry")

// accountKey builds the storage key of an account. A named account is stored
// under "host#username"; a host-wide token keeps the bare host, which is also
// the layout written by older versions.
func accountKey(registry, username string) string {
	host := CanonicalHost(registry)
	if username == "" || username == tokenUsername {
		return host
	}
	return host + "#" + username
}

// splitAccountKey is the inverse of accountKey; the username is empty for a
// bare host key, whose real username is read from the encoded entry.
func splitAccountKey(key string) (host, username string) {
	if i := strings.IndexByte(key, '#'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

// authFile is the on-disk format (Docker-compatible "auths" layout).
type authFile struct {
	Auths map[string]authEntry `json:"auths"`
}

type authEntry struct {
	Auth string `json:"auth,omitempty"` // base64(user:secret)
}

var errInsecurePerms = errors.New("credentials file has insecure permissions")

// CanonicalHost normalizes a registry host for use as a storage key.
// Docker Hub has three spellings that collapse to one.
func CanonicalHost(host string) string {
	h := strings.TrimSpace(host)
	h = strings.TrimSuffix(h, "/")
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h = strings.ToLower(h)
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "index.docker.io"
	}
	return h
}

// NewStore opens (creating if needed) the credential store at path.
// An existing file with permissions wider than 0600 is refused.
func NewStore(path string) (Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("credentials file path is empty")
	}
	s := &fileStore{path: path}
	if runtime.GOOS == "windows" {
		return s, nil
	}
	if st, err := os.Stat(path); err == nil {
		if st.Mode().Perm()&0o77 != 0 {
			return nil, fmt.Errorf("%w: %s is %04o: run chmod 600 %s", errInsecurePerms, path, st.Mode().Perm(), path)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	return s, nil
}

type fileStore struct {
	path string
}

func (s *fileStore) Get(key string) (*Credentials, error) {
	host := CanonicalHost(key)
	found, err := s.credentialsFor(host)
	if err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		c := found[0]
		return &c, nil
	default:
		return nil, fmt.Errorf("%w: %s has %d logins (%s)", ErrAmbiguousAccount, host, len(found), strings.Join(usernamesOf(found), ", "))
	}
}

func (s *fileStore) GetFor(key, username string) (*Credentials, error) {
	host := CanonicalHost(key)
	found, err := s.credentialsFor(host)
	if err != nil {
		return nil, err
	}
	for i := range found {
		if found[i].Username == username {
			c := found[i]
			return &c, nil
		}
	}
	return nil, nil
}

// credentialsFor decodes every account of one host, in a stable order.
func (s *fileStore) credentialsFor(host string) ([]Credentials, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(f.Auths))
	for k := range f.Auths {
		if h, _ := splitAccountKey(k); CanonicalHost(h) == host {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]Credentials, 0, len(keys))
	for _, k := range keys {
		c, err := decodeEntry(f.Auths[k].Auth)
		if err != nil {
			return nil, fmt.Errorf("decrypting credentials for %s: %w", k, err)
		}
		c.Registry = host
		out = append(out, c)
	}
	return out, nil
}

func usernamesOf(creds []Credentials) []string {
	out := make([]string, 0, len(creds))
	for _, c := range creds {
		if c.Username == tokenUsername {
			out = append(out, "token")
			continue
		}
		out = append(out, c.Username)
	}
	return out
}

func (s *fileStore) Put(c Credentials) error {
	f, err := s.read()
	if err != nil {
		return err
	}
	if f.Auths == nil {
		f.Auths = make(map[string]authEntry)
	}
	host := CanonicalHost(c.Registry)
	// The first account of a host keeps the bare host key, which is what Docker
	// and older backimage versions read; further accounts get a "host#user"
	// key. Re-logging in as a known user overwrites that user's own key.
	key := host
	if existing, ok := f.Auths[host]; ok {
		old, derr := decodeEntry(existing.Auth)
		if derr != nil || old.Username != c.Username {
			key = accountKey(host, c.Username)
		}
	}
	f.Auths[key] = authEntry{Auth: encodeEntry(c.Username, c.Secret)}
	return s.write(f)
}

func (s *fileStore) Delete(key string) error {
	f, err := s.read()
	if err != nil {
		return err
	}
	host := CanonicalHost(key)
	removed := false
	for k := range f.Auths {
		if h, _ := splitAccountKey(k); CanonicalHost(h) == host {
			delete(f.Auths, k)
			removed = true
		}
	}
	if !removed {
		return nil
	}
	return s.write(f)
}

func (s *fileStore) DeleteFor(key, username string) (bool, error) {
	f, err := s.read()
	if err != nil {
		return false, err
	}
	host := CanonicalHost(key)
	for k := range f.Auths {
		h, u := splitAccountKey(k)
		if CanonicalHost(h) != host {
			continue
		}
		if u == "" {
			// Bare host key: the username lives inside the entry.
			if c, derr := decodeEntry(f.Auths[k].Auth); derr == nil {
				u = c.Username
			}
		}
		if u == username || (username == "" && u == tokenUsername) {
			delete(f.Auths, k)
			return true, s.write(f)
		}
	}
	return false, nil
}

func (s *fileStore) Accounts() ([]Account, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(f.Auths))
	for k, e := range f.Auths {
		host, username := splitAccountKey(k)
		host = CanonicalHost(host)
		c, derr := decodeEntry(e.Auth)
		if derr == nil && username == "" {
			username = c.Username
		}
		acc := Account{Registry: host, Username: username}
		if username == tokenUsername {
			acc.Username, acc.Token = "", true
		}
		out = append(out, acc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Registry != out[j].Registry {
			return out[i].Registry < out[j].Registry
		}
		return out[i].Username < out[j].Username
	})
	return out, nil
}

func (s *fileStore) List() ([]string, error) {
	accounts, err := s.Accounts()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if seen[a.Registry] {
			continue
		}
		seen[a.Registry] = true
		out = append(out, a.Registry)
	}
	sort.Strings(out)
	return out, nil
}

// read loads the file, tolerating absence.
func (s *fileStore) read() (*authFile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &authFile{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	var f authFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	return &f, nil
}

// write persists atomically: temp file in the same directory, then rename.
func (s *fileStore) write(f *authFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating store directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".auth.json.tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmp.Name())
		}
	}()
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o600); err != nil {
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}
	committed = true
	return nil
}

func encodeEntry(user, secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
}

func decodeEntry(b64 string) (Credentials, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Credentials{}, fmt.Errorf("bad base64: %w", err)
	}
	i := strings.IndexByte(string(raw), ':')
	if i < 0 {
		return Credentials{}, errors.New("bad credentials encoding")
	}
	return Credentials{Username: string(raw[:i]), Secret: string(raw[i+1:])}, nil
}

// AnonymousUser is the value of the account selector that forces an
// unauthenticated request even when logins exist for that registry.
const AnonymousUser = "none"

// layeredKeychain tries explicit, then the local store, then Docker's
// config with its credential helpers, then anonymous.
type layeredKeychain struct {
	explicit *Credentials
	store    Store
	user     string // account selected explicitly; "" = derive from the repository
	docker   authn.Keychain
}

// NewKeychain builds the layered keychain used by backimage.
func NewKeychain(explicit *Credentials, store Store) Keychain {
	return NewKeychainForUser(explicit, store, "")
}

// NewKeychainForUser is NewKeychain with an explicitly selected account, the
// value of --registry-user. AnonymousUser forces an unauthenticated request.
func NewKeychainForUser(explicit *Credentials, store Store, user string) Keychain {
	return &layeredKeychain{
		explicit: explicit,
		store:    store,
		user:     strings.TrimSpace(user),
		docker:   authn.DefaultKeychain,
	}
}

// namespaceOf returns the first path element of a repository, the account name
// on registries that namespace repositories per user or organization
// (docker.io/user2/img -> "user2"). It is empty for a registry-wide resource.
func namespaceOf(res authn.Resource) string {
	type repositoryResource interface{ RepositoryStr() string }
	repo, ok := res.(repositoryResource)
	if !ok {
		return ""
	}
	path := strings.TrimPrefix(repo.RepositoryStr(), "/")
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

// selectAccount picks the stored login to use for a resource. The rule is
// deliberately strict: with logins on a host, a request either matches an
// account exactly or fails, because falling back to "some" account would push
// a backup under the wrong identity.
func (k *layeredKeychain) selectAccount(res authn.Resource) (*Credentials, error) {
	host := CanonicalHost(res.RegistryStr())
	accounts, err := k.store.Accounts()
	if err != nil {
		return nil, fmt.Errorf("resolve credentials for %s: %w", host, err)
	}
	names := make([]string, 0, len(accounts))
	hostAccounts := 0
	for _, a := range accounts {
		if a.Registry != host {
			continue
		}
		hostAccounts++
		if a.Token {
			names = append(names, "token")
			continue
		}
		names = append(names, a.Username)
	}
	if hostAccounts == 0 {
		return nil, nil // no login here: fall through to Docker/anonymous
	}

	if k.user != "" {
		if k.user == AnonymousUser {
			return nil, nil
		}
		c, err := k.store.GetFor(host, k.user)
		if err != nil {
			return nil, fmt.Errorf("resolve credentials for %s: %w", host, err)
		}
		if c == nil {
			return nil, fmt.Errorf("no login for %s as %q: stored accounts are %s", host, k.user, strings.Join(names, ", "))
		}
		return c, nil
	}

	if ns := namespaceOf(res); ns != "" {
		c, err := k.store.GetFor(host, ns)
		if err != nil {
			return nil, fmt.Errorf("resolve credentials for %s: %w", host, err)
		}
		if c != nil {
			return c, nil
		}
	}
	// A host-wide token carries no username and cannot be matched by
	// namespace; it is used when it is the only login for the host.
	if hostAccounts == 1 {
		if c, err := k.store.GetFor(host, tokenUsername); err == nil && c != nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("no login for %s matching %q: stored accounts are %s; select one with --registry-user NAME, or --registry-user %s for an anonymous request",
		host, namespaceOf(res), strings.Join(names, ", "), AnonymousUser)
}

func (k *layeredKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if k.explicit != nil && CanonicalHost(k.explicit.Registry) == CanonicalHost(res.RegistryStr()) {
		return authn.FromConfig(authConfig(*k.explicit)), nil
	}
	if k.store != nil {
		c, err := k.selectAccount(res)
		if err != nil {
			return nil, err
		}
		if c != nil && c.Secret != "" {
			return authn.FromConfig(authConfig(*c)), nil
		}
	}
	if k.user == AnonymousUser {
		return authn.FromConfig(authn.AuthConfig{}), nil
	}
	if k.docker != nil {
		a, err := k.docker.Resolve(res)
		if err != nil {
			return nil, fmt.Errorf("resolve Docker credentials for %s: %w", res.RegistryStr(), err)
		}
		if !isAnonymous(a) {
			return a, nil
		}
	}
	return authn.FromConfig(authn.AuthConfig{}), nil
}

func isAnonymous(a authn.Authenticator) bool {
	// authn.Anonymous is the canonical package-level empty authenticator.
	return a == authn.Anonymous
}
