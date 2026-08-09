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

// Store persists credentials in backimage's own file, mode 0600.
type Store interface {
	Get(registry string) (*Credentials, error)
	Put(c Credentials) error
	Delete(registry string) error
	List() ([]string, error) // registry hosts only, never secrets
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
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	e, ok := f.Auths[CanonicalHost(key)]
	if !ok {
		return nil, nil
	}
	c, err := decodeEntry(e.Auth)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials for %s: %w", key, err)
	}
	return &c, nil
}

func (s *fileStore) Put(c Credentials) error {
	f, err := s.read()
	if err != nil {
		return err
	}
	key := CanonicalHost(c.Registry)
	if f.Auths == nil {
		f.Auths = make(map[string]authEntry)
	}
	f.Auths[key] = authEntry{Auth: encodeEntry(c.Username, c.Secret)}
	return s.write(f)
}

func (s *fileStore) Delete(key string) error {
	f, err := s.read()
	if err != nil {
		return err
	}
	ck := CanonicalHost(key)
	if _, ok := f.Auths[ck]; !ok {
		return nil
	}
	delete(f.Auths, ck)
	return s.write(f)
}

func (s *fileStore) List() ([]string, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f.Auths))
	for k := range f.Auths {
		out = append(out, k)
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

// layeredKeychain tries explicit, then the local store, then Docker's
// config with its credential helpers, then anonymous.
type layeredKeychain struct {
	explicit *Credentials
	store    Store
	docker   authn.Keychain
}

// NewKeychain builds the layered keychain used by backimage.
func NewKeychain(explicit *Credentials, store Store) Keychain {
	return &layeredKeychain{
		explicit: explicit,
		store:    store,
		docker:   authn.DefaultKeychain,
	}
}

func (k *layeredKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if k.explicit != nil && CanonicalHost(k.explicit.Registry) == CanonicalHost(res.RegistryStr()) {
		return authn.FromConfig(authConfig(*k.explicit)), nil
	}
	if k.store != nil {
		c, err := k.store.Get(res.RegistryStr())
		if err != nil {
			return nil, fmt.Errorf("resolve credentials for %s: %w", res.RegistryStr(), err)
		}
		if c != nil && c.Secret != "" {
			return authn.FromConfig(authConfig(*c)), nil
		}
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
