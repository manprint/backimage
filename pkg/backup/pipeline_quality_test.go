package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	backarchive "github.com/fpierri/backimage/pkg/archive"
	"github.com/fpierri/backimage/pkg/chunk"
	"github.com/fpierri/backimage/pkg/compress"
	"github.com/fpierri/backimage/pkg/crypt"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/registry"
)

func TestPipelineHelpersAndTypeMapping(t *testing.T) {
	if ceilDiv(0, 10) != 0 || ceilDiv(10, 0) != 0 || ceilDiv(11, 10) != 2 {
		t.Fatal("ceilDiv edge cases")
	}
	want := map[backarchive.EntryType]string{
		backarchive.TypeRegular:     index.TypeRegular,
		backarchive.TypeDir:         index.TypeDir,
		backarchive.TypeSymlink:     index.TypeSymlink,
		backarchive.TypeHardlink:    index.TypeHardlink,
		backarchive.TypeCharDevice:  index.TypeChar,
		backarchive.TypeBlockDevice: index.TypeBlock,
		backarchive.TypeFifo:        index.TypeFifo,
	}
	for typ, code := range want {
		if got := typeCode(typ); got != code {
			t.Errorf("typeCode(%v) = %q, want %q", typ, got, code)
		}
	}
	if got := typeCode(backarchive.EntryType(255)); got != index.TypeRegular {
		t.Fatalf("unknown type fallback = %q", got)
	}

	b := &builder{cfg: Config{Platforms: []string{"amd64", "linux/arm64", "linux/arm/v7"}}}
	got := b.platforms()
	if len(got) != 3 || got[0].OS != "linux" || got[0].Architecture != "amd64" ||
		got[2].Variant != "v7" || platString(got[2]) != "linux/arm/v7" {
		t.Fatalf("platform parsing = %+v", got)
	}

	base := Config{Ref: "example.test/repo/backup:t", RootPaths: []string{"b", "a"}, Compression: "zstd", Version: "v1"}
	id1 := checkpointID(base)
	base.RootPaths = []string{"a", "b"}
	if id1 != checkpointID(base) {
		t.Fatal("checkpoint id depends on root ordering")
	}
	base.Encrypt = true
	if id1 == checkpointID(base) {
		t.Fatal("checkpoint id ignores encryption mode")
	}
	if refFor("example.test/repo/backup:t").Name() != "example.test/repo/backup:t" {
		t.Fatal("reference helper changed reference")
	}
}

func TestDedupLayerFallbackAndHardLimit(t *testing.T) {
	codec, err := compress.Get("zstd")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	spool, err := newSpool(dir, 110)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Write([]byte("layer data")); err != nil {
		t.Fatal(err)
	}
	layers := make([]index.LayerInfo, 109)
	layers[108].ChunkTo = 108
	b := &builder{
		cfg:           Config{Dedup: true, TempDir: dir},
		codec:         codec,
		level:         1,
		plan:          chunk.Plan{LayerBytes: 16 << 20},
		spool:         spool,
		layers:        layers,
		chunkIdx:      110,
		estimatedRaw:  512 << 20,
		plainBytes:    16 << 20,
		maxDataLayers: 118,
	}
	if err := b.rollLayer(); err != nil {
		t.Fatal(err)
	}
	defer b.cleanup()
	if !b.boundaryFallback {
		t.Fatal("dedup boundary fallback did not activate at 110 layers")
	}
	if b.shouldRollLayer([32]byte{}) {
		t.Fatal("no spool should never roll")
	}
	b.spool, err = newSpool(dir, 999)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.spool.Write([]byte("final layer")); err != nil {
		t.Fatal(err)
	}
	b.layers = make([]index.LayerInfo, 117)
	if b.shouldRollLayer([32]byte{}) {
		t.Fatal("the final OCI layer must remain available at the hard limit")
	}
}

func TestWalkEstimateFileCancellationAndDegradedError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "one")
	if err := os.WriteFile(file, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, size, err := walkEstimate(context.Background(), file, false)
	if err != nil || n != 1 || size != 5 {
		t.Fatalf("file estimate = %d, %d, %v", n, size, err)
	}
	if _, _, err := walkEstimate(context.Background(), file+"-missing", false); err == nil {
		t.Fatal("missing root accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := walkEstimate(ctx, filepath.Dir(file), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestDryRunDoesNotPromptOrCreateCredentialDirectory(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(t.TempDir(), "missing", "auth.json")
	store, err := registryStore(authPath)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = Run(context.Background(), Config{
		RootPaths: []string{tree}, Ref: "localhost:1/no/network:t", Compression: "zstd",
		Encrypt: true, Passphrase: func() ([]byte, error) { called = true; return []byte("secret"), nil },
		AllowDegraded: true, DryRun: true, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("dry-run prompted for a passphrase")
	}
	if _, err := os.Stat(filepath.Dir(authPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created credential directory: %v", err)
	}
}

// registryStore is kept local so this test exercises the real lazy Store
// constructor without coupling the test body to its concrete type.
func registryStore(path string) (registry.Store, error) {
	return registry.NewStore(path)
}

func TestPipelineTarRecipientAndTempCleanup(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "secret.txt"), []byte(strings.Repeat("secret", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "backup.tar")
	res, err := Run(context.Background(), Config{
		RootPaths: []string{tree}, Ref: "example.test/repo/backup:t", Compression: "gzip", Level: 1,
		Encrypt: true, Recipients: []string{id.Recipient().String()}, AllowDegraded: true,
		Runnable: false, Platforms: []string{"linux/amd64"}, SelfExtract: stubSelf,
		Output: "tar", OutputPath: out, TempDir: tempDir,
		NoMetadata: true, Created: "2026-01-02T03:04:05Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Encrypted || res.BytesStored == 0 {
		t.Fatalf("result = %+v", res)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() == 0 {
		t.Fatalf("tar output = %v, %v", st, err)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "backimage-layer-") {
			t.Fatalf("temporary layer leaked: %s", entry.Name())
		}
	}
}

func TestBuilderKeyFilesAndCleanupAreIdempotent(t *testing.T) {
	km, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()
	codec, err := compress.Get("zstd")
	if err != nil {
		t.Fatal(err)
	}
	b := &builder{cfg: Config{Encrypt: true}, km: km, codec: codec, passphrase: []byte("pw")}
	files, err := b.buildKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files["keys.pass.age"]) == 0 {
		t.Fatal("passphrase key file missing")
	}
	emptyBuilder := &builder{cfg: Config{Encrypt: true}, km: km}
	emptyFiles, err := emptyBuilder.buildKeyFiles()
	if err != nil || len(emptyFiles) != 0 {
		t.Fatalf("empty key files = %v, %v", emptyFiles, err)
	}
	b.cleanup()
	b.cleanup()
	if !b.cleaned {
		t.Fatal("cleanup did not mark builder clean")
	}

	m := manifestJSON(&index.Manifest{SchemaVersion: 1, CreatedAt: time.Unix(0, 0).UTC()})
	if len(m) == 0 {
		t.Fatal("manifestJSON returned empty output")
	}
}

func TestPipelineFailureBranches(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := Config{
		RootPaths: []string{tree}, Ref: "example.test/repo/backup:t", Compression: "zstd",
		AllowDegraded: true, Output: "oci-layout", OutputPath: filepath.Join(t.TempDir(), "out"),
		Runnable: false, Platforms: []string{"linux/amd64"}, SelfExtract: stubSelf,
	}

	badCodec := base
	badCodec.Compression = "not-a-codec"
	if _, err := Run(context.Background(), badCodec); err == nil {
		t.Fatal("unknown codec accepted")
	}

	passErr := base
	passErr.Encrypt = true
	passErr.Passphrase = func() ([]byte, error) { return nil, errors.New("prompt failed") }
	if _, err := Run(context.Background(), passErr); err == nil || !strings.Contains(err.Error(), "prompt failed") {
		t.Fatalf("passphrase error = %v", err)
	}

	selfErr := base
	selfErr.SelfExtract = func(string) ([]byte, error) { return nil, errors.New("no executable") }
	if _, err := Run(context.Background(), selfErr); err == nil || !strings.Contains(err.Error(), "no executable") {
		t.Fatalf("self-extract error = %v", err)
	}
	missingSelf := base
	missingSelf.SelfExtract = nil
	if _, err := Run(context.Background(), missingSelf); err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("missing self-extract error = %v", err)
	}

	badRecipient := base
	badRecipient.Encrypt = true
	badRecipient.Recipients = []string{"not-an-age-recipient"}
	if _, err := Run(context.Background(), badRecipient); err == nil {
		t.Fatal("invalid age recipient accepted")
	}

	oldStatfs := statfs
	statfs = func(string) (int64, error) { return 0, errors.New("statfs failed") }
	t.Cleanup(func() { statfs = oldStatfs })
	if _, err := Run(context.Background(), base); err == nil || !strings.Contains(err.Error(), "statfs failed") {
		t.Fatalf("statfs error = %v", err)
	}
}

func TestRegistryPreflightFailsBeforeEstimate(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "source-does-not-exist")
	_, err := Run(context.Background(), Config{
		RootPaths: []string{missing}, Ref: "localhost:1/repo/backup:t", Compression: "zstd",
		AllowDegraded: true, Output: "registry",
	})
	if err == nil || strings.Contains(err.Error(), "stima") {
		t.Fatalf("registry preflight should fail before source estimate, got %v", err)
	}
}

func TestRunValidationAndEstimateErrors(t *testing.T) {
	if _, err := Run(context.Background(), Config{}); err == nil {
		t.Fatal("invalid empty config accepted")
	}
	_, err := Run(context.Background(), Config{
		RootPaths: []string{filepath.Join(t.TempDir(), "missing")},
		Ref:       "example.test/repo/backup:t", Compression: "zstd",
		AllowDegraded: true, DryRun: true, Output: "oci-layout", OutputPath: filepath.Join(t.TempDir(), "out"),
	})
	if err == nil || !strings.Contains(err.Error(), "stima") {
		t.Fatalf("missing source estimate error = %v", err)
	}
}

func TestSpoolFailureAndUnknownLocalOutput(t *testing.T) {
	if _, err := newSpool(filepath.Join(t.TempDir(), "missing"), 0); err == nil {
		t.Fatal("spool in missing directory accepted")
	}
	file, err := os.CreateTemp(t.TempDir(), "spool")
	if err != nil {
		t.Fatal(err)
	}
	sp := &spoolFile{path: file.Name(), f: file}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp.Write([]byte("x")); err == nil {
		t.Fatal("write to closed spool succeeded")
	}
	if _, err := streamDigest(sp); err == nil {
		t.Fatal("digest of closed spool succeeded")
	}
	sp.Remove()

	b := &builder{cfg: Config{Output: "unknown"}}
	if err := b.pushLocal(context.Background(), nil, nil); err != nil {
		t.Fatalf("unknown local output should be a no-op after validation, got %v", err)
	}
}
