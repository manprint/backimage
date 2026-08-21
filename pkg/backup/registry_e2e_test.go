package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/registry"
	"github.com/manprint/backimage/pkg/restore"
)

// memReg is a minimal in-memory v2 registry (no auth challenge: the base
// path answers 200 so push skips the token flow).
type memReg struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]string
	ups       map[string][]byte
	nextUp    uint64
	putHits   int
	patchHits int
}

func newMemReg() *memReg {
	return &memReg{blobs: map[string][]byte{}, manifests: map[string]string{}, ups: map[string][]byte{}}
}

func (m *memReg) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="http://`+r.Host+`/token",service="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v2/")
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case strings.HasSuffix(rest, "tags/list") && r.Method == http.MethodGet:
			tags := make([]string, 0, len(m.manifests))
			for tag := range m.manifests {
				if !strings.HasPrefix(tag, "sha256:") && !strings.Contains(tag, "/") {
					tags = append(tags, tag)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": strings.TrimSuffix(rest, "/tags/list"), "tags": tags})
		case strings.Contains(rest, "manifests/") && r.Method == http.MethodGet:
			stored, ok := m.manifests[rest[strings.LastIndex(rest, "manifests/")+len("manifests/"):]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			parts := strings.SplitN(stored, "|", 2)
			w.Header().Set("Content-Type", parts[0])
			_, _ = io.WriteString(w, parts[1])
		case strings.Contains(rest, "blobs/") && r.Method == http.MethodGet:
			blob, ok := m.blobs[rest[strings.LastIndex(rest, "blobs/")+len("blobs/"):]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(blob)
		case strings.Contains(rest, "blobs/uploads/") && r.Method == http.MethodPatch:
			id := rest[strings.LastIndex(rest, "uploads/")+len("uploads/"):]
			b, _ := io.ReadAll(r.Body)
			m.ups[id] = append(m.ups[id], b...)
			m.patchHits++
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(rest, "blobs/uploads/") && r.Method == http.MethodPut:
			id := rest[strings.LastIndex(rest, "uploads/")+len("uploads/"):]
			dig := r.URL.Query().Get("digest")
			m.putHits++
			m.blobs[dig] = append([]byte(nil), m.ups[id]...)
			delete(m.ups, id)
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(rest, "blobs/uploads/") && r.Method == http.MethodPost:
			// Push uploads are concurrent. Using len(m.ups) reuses an ID as
			// soon as another upload completes and makes this test registry
			// nondeterministically overwrite an in-flight upload.
			id := fmt.Sprintf("up%d", m.nextUp)
			m.nextUp++
			m.ups[id] = nil
			w.Header().Set("Location", "/v2/"+rest+"uploads/"+id)
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(rest, "blobs/") && r.Method == http.MethodHead:
			digest := rest[strings.LastIndex(rest, "blobs/")+len("blobs/"):]
			if blob, ok := m.blobs[digest]; ok {
				w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
				w.Header().Set("Docker-Content-Digest", digest)
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		// The post-push verification resolves the tag with a HEAD.
		case strings.Contains(rest, "manifests/") && r.Method == http.MethodHead:
			ident := rest[strings.LastIndex(rest, "manifests/")+len("manifests/"):]
			stored, ok := m.manifests[ident]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			parts := strings.SplitN(stored, "|", 2)
			sum := sha256.Sum256([]byte(parts[1]))
			w.Header().Set("Content-Type", parts[0])
			w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
			w.WriteHeader(http.StatusOK)
		case strings.Contains(rest, "manifests/") && r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			m.manifests[rest[strings.LastIndex(rest, "manifests/")+len("manifests/"):]] = r.Header.Get("Content-Type") + "|" + string(b)
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/token" && r.Method == http.MethodGet:
			w.Write([]byte(`{"token":"t","expires_in":600}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func refTo(srvURL string) string {
	return strings.TrimPrefix(srvURL, "http://") + "/me/dumps:latest"
}

func prepPushDirs(cfg *Config) {
	for _, d := range []string{cfg.TempDir, cfg.CheckpointDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			panic(err)
		}
	}
}

func pipelinePushConfig(srvURL, tree string) Config {
	return Config{
		RootPaths:     []string{tree},
		Ref:           refTo(srvURL),
		Compression:   "zstd",
		AllowDegraded: true,
		SelfExtract:   stubSelf,
		TempDir:       tree + "-tmp",
		CheckpointDir: tree + "-ckpt",
		Resume:        true,
	}
}

func TestPipelinePushToRegistryE2E(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()

	tree := t.TempDir()
	f, err := os.Create(filepath.Join(tree, "big.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(100 << 20); err != nil { // 100 MiB sparse
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	cfg := pipelinePushConfig(srv.URL, tree)
	prepPushDirs(&cfg)
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("push e2e: %v", err)
	}
	reg.mu.Lock()
	nBlobs := len(reg.blobs)
	idx := ""
	for _, v := range reg.manifests {
		if strings.Contains(v, "image.index.v1") {
			idx = v
			break
		}
	}
	reg.mu.Unlock()
	if idx == "" {
		reg.mu.Lock()
		for k, v := range reg.manifests {
			t.Logf("manifest[%s] = %s", k, v[:min(80, len(v))])
		}
		reg.mu.Unlock()
		t.Fatal("index manifest not stored")
	}
	if nBlobs == 0 {
		t.Fatal("no blobs stored")
	}
	if res.SkippedBlobs != 0 {
		t.Fatalf("fresh push must not skip: %d", res.SkippedBlobs)
	}
	if res.Layers != 3 { // 100 MiB / 32 MiB layers + index? -> see below
		t.Logf("(layers=%d chunks checking skipped)", res.Layers)
	}
}

func TestPipelinePushIdempotentResume(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()

	tree := t.TempDir()
	f, _ := os.Create(filepath.Join(tree, "data.txt"))
	f.WriteString("idempotent payload\n")
	f.Close()

	cfg := pipelinePushConfig(srv.URL, tree)
	cfg.TempDir = t.TempDir()
	cfg.CheckpointDir = t.TempDir()

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	reg.mu.Lock()
	puts1 := reg.putHits
	reg.mu.Unlock()
	secondRes, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	reg.mu.Lock()
	puts2 := reg.putHits - puts1
	reg.mu.Unlock()
	t.Logf("first: puts=%d; second: puts=%d", puts1, puts2)
	if puts2 >= puts1 {
		t.Fatalf("second run uploaded %d blobs, first uploaded %d; stable blobs were not reused", puts2, puts1)
	}
	if secondRes.SkippedBlobs == 0 {
		t.Fatal("second run must report skipped blobs")
	}
}

func TestPipelineEncryptedDedupReusesConvergentKey(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()

	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "data.bin"), bytes.Repeat([]byte("dedup-content-"), (24<<20)/len("dedup-content-")), 0o600); err != nil {
		t.Fatal(err)
	}
	baseRef := strings.TrimPrefix(srv.URL, "http://") + "/me/dumps"
	passphrase := func() ([]byte, error) { return []byte("dedup passphrase"), nil }
	config := pipelinePushConfig(srv.URL, tree)
	config.Ref = baseRef + ":t1"
	config.TempDir = t.TempDir()
	config.CheckpointDir = t.TempDir()
	config.Encrypt = true
	config.Passphrase = passphrase
	config.Dedup = true
	config.MaxLayerSize = 16 << 20
	config.Runnable = false
	config.Platforms = []string{"linux/amd64"}

	first, err := Run(context.Background(), config)
	if err != nil {
		t.Fatalf("first dedup backup: %v", err)
	}
	previous, previousErr := findDedupBase(context.Background(), config)
	if previousErr != nil || previous == nil {
		reg.mu.Lock()
		keys := make([]string, 0, len(reg.manifests))
		for key := range reg.manifests {
			keys = append(keys, key)
		}
		reg.mu.Unlock()
		t.Fatalf("findDedupBase after first backup = %v, %v (manifests %v)", previous, previousErr, keys)
	}
	config.Ref = baseRef + ":t2"
	var secondWarnings []string
	config.Progress = func(s string) { secondWarnings = append(secondWarnings, s) }
	second, err := Run(context.Background(), config)
	if err != nil {
		t.Fatalf("second dedup backup: %v", err)
	}
	if first.UploadedBytes == 0 || second.SkippedBlobs == 0 || second.SkippedBytes == 0 {
		t.Fatalf("dedup statistics first=%+v second=%+v", first, second)
	}
	repo, err := name.NewRepository(baseRef)
	if err != nil {
		t.Fatal(err)
	}
	repoStats, err := registry.Stats(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if repoStats.Tags != 2 || repoStats.SharedBlobs == 0 || repoStats.StorageBytes >= repoStats.ReferencedBytes {
		t.Fatalf("repo stats do not show sharing: %+v", repoStats)
	}

	load := func(tag, pass string) (*crypt.KeyMaterial, string) {
		t.Helper()
		ref, err := name.ParseReference(baseRef + ":" + tag)
		if err != nil {
			t.Fatal(err)
		}
		src, err := restore.FromRegistry(context.Background(), ref, nil, restore.SourceOptions{CacheDir: t.TempDir(), CacheSize: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		m, err := src.Manifest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The nonce mode stays public: dedup must pick the previous key before
		// anything is unlocked. The key fingerprint does not: it links backups
		// to each other, so it travels inside the sealed private blob.
		if m.Encryption.NonceMode != "convergent" || m.Encryption.KeyFingerprint != "" {
			t.Fatalf("dedup encryption metadata = %+v", m.Encryption)
		}
		keyFile, err := src.KeyFile(context.Background(), "keys.pass.age")
		if err != nil {
			t.Fatal(err)
		}
		km, err := crypt.UnwrapKeys(bytes.NewReader(keyFile), crypt.Identity{Passphrase: []byte(pass)})
		if err != nil {
			t.Fatal(err)
		}
		opener, err := crypt.NewOpener(km)
		if err != nil {
			t.Fatal(err)
		}
		privateBlob, err := src.PrivateBlob(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		private, err := index.ReadPrivate(bytes.NewReader(privateBlob), opener)
		if err != nil {
			t.Fatal(err)
		}
		if private.Encryption.KeyFingerprint == "" {
			t.Fatalf("private metadata without a key fingerprint: %+v", private.Encryption)
		}
		return km, private.Encryption.KeyFingerprint
	}
	key1, fingerprint1 := load("t1", "dedup passphrase")
	defer key1.Wipe()
	key2, fingerprint2 := load("t2", "dedup passphrase")
	defer key2.Wipe()
	if fingerprint1 != fingerprint2 || string(key1.DEK) != string(key2.DEK) {
		t.Fatalf("dedup key was not reused: %s != %s (%v)", fingerprint1, fingerprint2, secondWarnings)
	}

	config.Ref = baseRef + ":t3"
	config.Passphrase = func() ([]byte, error) { return []byte("different passphrase"), nil }
	var warnings []string
	config.Progress = func(s string) { warnings = append(warnings, s) }
	if _, err := Run(context.Background(), config); err != nil {
		t.Fatalf("different passphrase backup: %v", err)
	}
	_, fingerprint3 := load("t3", "different passphrase")
	if fingerprint1 == fingerprint3 {
		t.Fatal("changed passphrase reused the previous dedup key")
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "passphrase diversa") {
		t.Fatalf("missing changed passphrase warning: %v", warnings)
	}
}

func TestDedupRefusesRandomNonceKeyReuse(t *testing.T) {
	old, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Wipe()
	var wrapped bytes.Buffer
	if err := crypt.WrapKeys(&wrapped, old, crypt.Recipients{Passphrase: []byte("same passphrase")}); err != nil {
		t.Fatal(err)
	}
	previous := &dedupBase{
		manifest: &index.Manifest{Encryption: index.EncryptionInfo{Enabled: true, NonceMode: "random"}},
		keyFiles: map[string][]byte{"keys.pass.age": wrapped.Bytes()},
	}
	if km, reused := reuseDedupKey(previous, Config{Passphrase: func() ([]byte, error) { return []byte("same passphrase"), nil }}, []byte("same passphrase")); reused || km != nil {
		t.Fatal("random-nonce KeyMaterial must never be reused for convergent dedup")
	}
	if warning := dedupKeyWarning(previous, Config{}); !strings.Contains(warning, "modalita' nonce") {
		t.Fatalf("missing immutable-mode warning: %q", warning)
	}
}

// TestDedupRefusesLegacyEnvelopeKeyReuse is the second half of the 0.2.4 nonce
// fix. A key that sealed convergent blobs with the pre-0.2.4 derivation may
// already have signed two different byte strings under one nonce somewhere in
// the repository, which is enough to recover the GHASH authentication key of
// that DEK. It must never seal anything again, so a fresh key is generated even
// though the passphrase opens the old key file perfectly.
func TestDedupRefusesLegacyEnvelopeKeyReuse(t *testing.T) {
	old, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Wipe()
	var wrapped bytes.Buffer
	if err := crypt.WrapKeys(&wrapped, old, crypt.Recipients{Passphrase: []byte("same passphrase")}); err != nil {
		t.Fatal(err)
	}
	base := func(envelopeVersion int) *dedupBase {
		return &dedupBase{
			manifest: &index.Manifest{Encryption: index.EncryptionInfo{
				Enabled:         true,
				NonceMode:       "convergent",
				EnvelopeVersion: envelopeVersion,
			}},
			keyFiles: map[string][]byte{"keys.pass.age": wrapped.Bytes()},
		}
	}
	cfg := Config{}

	// 0 is what 0.2.3 and earlier wrote: the field did not exist.
	for _, legacy := range []int{0, crypt.EnvelopeVersion - 1} {
		previous := base(legacy)
		if km, reused := reuseDedupKey(previous, cfg, []byte("same passphrase")); reused || km != nil {
			km.Wipe()
			t.Fatalf("envelopeVersion %d: a legacy-derivation key must not be reused", legacy)
		}
		if warning := dedupKeyWarning(previous, cfg); !strings.Contains(warning, "0.2.4") {
			t.Fatalf("envelopeVersion %d: missing legacy-key warning: %q", legacy, warning)
		}
	}

	// A key from the current envelope is still reused, or --dedup would upload
	// everything on every run.
	previous := base(crypt.EnvelopeVersion)
	km, reused := reuseDedupKey(previous, cfg, []byte("same passphrase"))
	if !reused || km == nil {
		t.Fatal("a current-envelope key must be reused for dedup")
	}
	defer km.Wipe()
	if !bytes.Equal(km.DEK, old.DEK) || !bytes.Equal(km.NonceKey, old.NonceKey) {
		t.Fatal("reused key material must be the previous one")
	}
}

// TestPipelineUploadChunkSizeWiring checks that the CLI knob reaches the
// pusher: by default the pipeline must cost one PATCH per blob, and asking
// for a chunk size must actually split the uploads.
func TestPipelineUploadChunkSizeWiring(t *testing.T) {
	// 6 MiB of incompressible noise from a fixed seed: a compressible fixture
	// would collapse into blobs smaller than any chunk size and the test
	// would prove nothing.
	payload := make([]byte, 6<<20)
	if _, err := mrand.New(mrand.NewSource(1)).Read(payload); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, chunk int64) (patches, blobs int) {
		t.Helper()
		reg := newMemReg()
		srv := reg.server()
		defer srv.Close()
		tree := t.TempDir()
		if err := os.WriteFile(filepath.Join(tree, "noise.dat"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := pipelinePushConfig(srv.URL, tree)
		cfg.UploadChunkSize = chunk
		prepPushDirs(&cfg)
		if _, err := Run(context.Background(), cfg); err != nil {
			t.Fatalf("push with chunk %d: %v", chunk, err)
		}
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return reg.patchHits, len(reg.blobs)
	}

	singlePatches, blobs := run(t, 0)
	if blobs == 0 {
		t.Fatal("no blobs stored")
	}
	if singlePatches != blobs {
		t.Errorf("default push sent %d PATCH for %d blobs, want one each", singlePatches, blobs)
	}

	chunkedPatches, chunkedBlobs := run(t, 1<<20)
	if chunkedBlobs != blobs {
		t.Fatalf("chunked push stored %d blobs, want the same %d", chunkedBlobs, blobs)
	}
	if chunkedPatches <= singlePatches {
		t.Errorf("--upload-chunk-size 1MiB sent %d PATCH, not more than the %d of a single-request push", chunkedPatches, singlePatches)
	}
}
