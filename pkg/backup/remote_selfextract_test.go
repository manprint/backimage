package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/manprint/backimage/pkg/registry"
	backremote "github.com/manprint/backimage/pkg/remote"
)

// otherBuildSelf is the extractor of a listen-remote built from another
// revision: same protocol, different bytes.
func otherBuildSelf(goarch string) ([]byte, error) {
	return []byte("ELF-OTHER-BUILD-" + goarch), nil
}

// layersBackupConfig is a protocol v1 backup of a small tree, with the
// checkpoint enabled as the CLI enables it by default.
func layersBackupConfig(t *testing.T, ref string, uploader RemoteUploader, warn func(string)) (Config, string) {
	t.Helper()
	tree := t.TempDir()
	// Incompressible and above two layers, so the image has several data
	// layers behind the metadata one.
	data := make([]byte, 5<<19)
	if _, err := mrand.New(mrand.NewSource(11)).Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "data.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoints := t.TempDir()
	return Config{
		RootPaths:     []string{tree},
		Ref:           ref,
		Compression:   "zstd",
		Level:         1,
		Jobs:          2,
		MaxLayerSize:  1 << 20,
		AllowDegraded: true,
		Encrypt:       true,
		Passphrase:    func() ([]byte, error) { return []byte("layers passphrase"), nil },
		Platforms:     []string{"linux/amd64", "linux/arm64"},
		TempDir:       t.TempDir(),
		Resume:        true,
		CheckpointDir: checkpoints,
		SelfExtract:   stubSelf,
		Remote:        uploader,
		Warn:          warn,
	}, checkpoints
}

// taggedDigest is the digest of the index the registry serves for the tag.
func (m *memReg) taggedDigest(t *testing.T, tag string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.manifests[tag]
	if !ok {
		t.Fatalf("tag %q not on the registry", tag)
	}
	sum := sha256.Sum256([]byte(strings.SplitN(stored, "|", 2)[1]))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestRemoteLayersRepublishesOwnSelfExtract is the version-skew regression:
// a v1 receiver of another build puts its own extractor in layer 0, which
// used to fail every such backup with an integrity error after the tag had
// been pushed. The client must end with its own image on the tag — never
// the receiver's layer 0, the program docker run executes — and report
// success with the receiver's counters.
func TestRemoteLayersRepublishesOwnSelfExtract(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()
	ref := refTo(srv.URL)

	var mu sync.Mutex
	var warnings []string
	warn := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, line)
	}
	cfg, checkpoints := layersBackupConfig(t, ref, remoteClient(t, ref, srv.URL, otherBuildSelf), warn)
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("layers backup against a receiver of another build: %v", err)
	}
	if got := reg.taggedDigest(t, "latest"); got != res.Digest {
		t.Fatalf("tag resolves to %s, the client built %s", got, res.Digest)
	}
	if !res.SelfExtractRepublished {
		t.Fatal("the result does not record the republish")
	}

	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := remote.Index(parsed)
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Manifests) != 2 {
		t.Fatalf("published platforms = %d", len(im.Manifests))
	}
	for _, desc := range im.Manifests {
		img, err := idx.Image(desc.Digest)
		if err != nil {
			t.Fatal(err)
		}
		layers, err := img.Layers()
		if err != nil {
			t.Fatal(err)
		}
		want, _ := stubSelf(desc.Platform.Architecture)
		if got := layerFile(t, layers[0], "backimage"); !bytes.Equal(got, want) {
			t.Fatalf("%s layer 0 holds %q, want the client's extractor %q", desc.Platform, got, want)
		}
	}

	// The counters describe the receiver's upload; the repair re-checks every
	// data blob and must not count them a second time as skipped.
	if res.SkippedBlobs != 0 || res.SkippedBytes != 0 || res.UploadedBytes != 0 {
		t.Fatalf("counters = skipped %d (%d bytes), uploaded %d bytes", res.SkippedBlobs, res.SkippedBytes, res.UploadedBytes)
	}
	entries, err := os.ReadDir(checkpoints)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the repair wrote checkpoints: %v", entries)
	}
	// A warning, not a progress line: the CLI keeps it under --quiet, where
	// unattended runs live.
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(warnings, "\n"), "/backimage") {
		t.Fatalf("no warning about the receiver's extractor in %q", warnings)
	}
}

// invalidDigestUploader stands for a receiver that reports a digest that is
// not one.
type invalidDigestUploader struct{}

func (invalidDigestUploader) Upload(context.Context, backremote.Backup) (backremote.Result, error) {
	return backremote.Result{Digest: "sha256:zz"}, nil
}

// TestRemoteLayersInvalidDigestIsAnIntegrityFailure: nothing can be read back
// by a digest that does not parse, so there is nothing to compare and the
// client must not republish.
func TestRemoteLayersInvalidDigestIsAnIntegrityFailure(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()
	cfg, _ := layersBackupConfig(t, refTo(srv.URL), invalidDigestUploader{}, nil)
	_, err := Run(context.Background(), cfg)
	if !errors.Is(err, registry.ErrPublishedMismatch) {
		t.Fatalf("err = %v, want ErrPublishedMismatch", err)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.manifests) != 0 {
		t.Fatalf("the client published %d manifests after an invalid digest", len(reg.manifests))
	}
}

// otherDataUploader stands for a receiver that publishes an image unrelated
// to what the client sent and reports its digest.
type otherDataUploader struct {
	ref    string
	digest string
}

func (u *otherDataUploader) Upload(ctx context.Context, _ backremote.Backup) (backremote.Result, error) {
	parsed, err := name.ParseReference(u.ref)
	if err != nil {
		return backremote.Result{}, err
	}
	images := map[string]v1.Image{}
	adds := []mutate.IndexAddendum{}
	for _, arch := range []string{"amd64", "arm64"} {
		img, err := random.Image(1024, 3)
		if err != nil {
			return backremote.Result{}, err
		}
		images["linux/"+arch] = img
		adds = append(adds, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
	}
	idx := mutate.AppendManifests(empty.Index, adds...)
	if err := registry.Push(ctx, parsed, images, idx, nil, registry.PushOptions{}); err != nil {
		return backremote.Result{}, err
	}
	digest, err := idx.Digest()
	if err != nil {
		return backremote.Result{}, err
	}
	u.digest = digest.String()
	return backremote.Result{Digest: u.digest}, nil
}

// TestRemoteLayersOtherDataIsNotRepaired keeps the integrity failure for
// what version skew cannot explain: the client must not paper over a
// receiver that published other data by republishing its own image.
func TestRemoteLayersOtherDataIsNotRepaired(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()
	ref := refTo(srv.URL)
	uploader := &otherDataUploader{ref: ref}
	cfg, _ := layersBackupConfig(t, ref, uploader, nil)
	_, err := Run(context.Background(), cfg)
	if !errors.Is(err, registry.ErrPublishedMismatch) {
		t.Fatalf("err = %v, want ErrPublishedMismatch", err)
	}
	if got := reg.taggedDigest(t, "latest"); got != uploader.digest {
		t.Fatalf("tag moved to %s: the client republished over a receiver that published other data", got)
	}
}

// freezingUploader runs the real receiver, then makes the registry refuse
// every write, so the client's republish fails.
type freezingUploader struct {
	inner  RemoteUploader
	frozen *atomic.Bool
}

func (u *freezingUploader) Upload(ctx context.Context, b backremote.Backup) (backremote.Result, error) {
	res, err := u.inner.Upload(ctx, b)
	u.frozen.Store(true)
	return res, err
}

// TestRemoteLayersFailedRepairNamesTheTag: when the republish fails the tag
// still holds the receiver's extractor, and the error has to say so in the
// terms an operator acts on, not only as a digest mismatch.
func TestRemoteLayersFailedRepairNamesTheTag(t *testing.T) {
	reg := newMemReg()
	var frozen atomic.Bool
	handler := reg.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if frozen.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ref := refTo(srv.URL)
	uploader := &freezingUploader{inner: remoteClient(t, ref, srv.URL, otherBuildSelf), frozen: &frozen}
	cfg, _ := layersBackupConfig(t, ref, uploader, nil)
	_, err := Run(context.Background(), cfg)
	if !errors.Is(err, registry.ErrPublishedMismatch) {
		t.Fatalf("err = %v, want ErrPublishedMismatch", err)
	}
	if !strings.Contains(err.Error(), "docker run") {
		t.Fatalf("error does not warn about the receiver's extractor: %v", err)
	}
}

// TestOnlySelfExtractDiffers pins what counts as version skew: layer 0 and
// whatever derives from it, nothing else.
func TestOnlySelfExtractDiffers(t *testing.T) {
	layer := func(s string) v1.Layer { return static.NewLayer([]byte(s), types.OCIUncompressedLayer) }
	image := func(t *testing.T, contents ...string) v1.Image {
		t.Helper()
		img := empty.Image
		for _, c := range contents {
			var err error
			if img, err = mutate.AppendLayers(img, layer(c)); err != nil {
				t.Fatal(err)
			}
		}
		return img
	}
	type plat struct {
		arch   string
		layers []string
	}
	index := func(t *testing.T, plats ...plat) v1.ImageIndex {
		t.Helper()
		adds := make([]mutate.IndexAddendum, 0, len(plats))
		for _, p := range plats {
			adds = append(adds, mutate.IndexAddendum{
				Add:        image(t, p.layers...),
				Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: p.arch}},
			})
		}
		return mutate.AppendManifests(empty.Index, adds...)
	}
	local := []plat{
		{"amd64", []string{"self-amd64", "meta", "data-1", "data-2"}},
		{"arm64", []string{"self-arm64", "meta", "data-1", "data-2"}},
	}
	cases := []struct {
		name      string
		published []plat
		ok        bool
	}{
		{"identical", local, true},
		{"only layer 0", []plat{
			{"arm64", []string{"other-arm64", "meta", "data-1", "data-2"}},
			{"amd64", []string{"other-amd64", "meta", "data-1", "data-2"}},
		}, true},
		{"metadata layer", []plat{
			{"amd64", []string{"self-amd64", "meta-x", "data-1", "data-2"}},
			{"arm64", []string{"self-arm64", "meta", "data-1", "data-2"}},
		}, false},
		{"data layer", []plat{
			{"amd64", []string{"self-amd64", "meta", "data-1", "data-2"}},
			{"arm64", []string{"self-arm64", "meta", "data-1", "data-x"}},
		}, false},
		{"missing platform", local[:1], false},
		{"extra platform", append(append([]plat{}, local...), plat{"riscv64", []string{"self", "meta", "data-1", "data-2"}}), false},
		{"platform replaced", []plat{
			local[0],
			{"riscv64", []string{"self-arm64", "meta", "data-1", "data-2"}},
		}, false},
		{"duplicated platform", []plat{local[0], local[0]}, false},
		{"layer count", []plat{
			{"amd64", []string{"self-amd64", "meta", "data-1"}},
			{"arm64", []string{"self-arm64", "meta", "data-1", "data-2"}},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := onlySelfExtractDiffers(index(t, local...), index(t, tc.published...))
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// layerFile returns one file of an uncompressed-readable layer.
func layerFile(t *testing.T, layer v1.Layer, want string) []byte {
	t.Helper()
	rc, err := layer.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("%s not in the layer", want)
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(h.Name, "/") == want {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
}
