package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/ociimg"
)

type countedLayer struct {
	v1.Layer
	compressed atomic.Int32
}

type layersImage struct {
	v1.Image
	layers []v1.Layer
	err    error
}

func (i *layersImage) Layers() ([]v1.Layer, error) {
	if i.err != nil {
		return nil, i.err
	}
	return i.layers, nil
}

type failingLayer struct {
	v1.Layer
	uncompressedErr error
}

func (l *failingLayer) Uncompressed() (io.ReadCloser, error) {
	if l.uncompressedErr != nil {
		return nil, l.uncompressedErr
	}
	return io.NopCloser(bytes.NewReader([]byte("not a tar"))), nil
}

func (l *countedLayer) Compressed() (io.ReadCloser, error) {
	l.compressed.Add(1)
	return l.Layer.Compressed()
}

func sourceFixture(t *testing.T) (v1.Image, *countedLayer, []byte, []byte) {
	t.Helper()
	codec, err := compress.Get("store")
	if err != nil {
		t.Fatal(err)
	}
	a, b := []byte("first stored chunk"), []byte("second stored chunk")
	combined := append(append([]byte(nil), a...), b...)
	base, err := ociimg.NewLayer([]ociimg.LayerFile{{Path: "/backup/data/000000.blob", Mode: 0o644, Size: int64(len(combined)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(combined)), nil }}}, codec, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := &countedLayer{Layer: base}
	rows := []index.Chunk{
		chunkRow(0, a), chunkRow(1, b),
	}
	m := &index.Manifest{
		SchemaVersion: 1, Tool: index.ToolInfo{Name: "backimage", Version: "test"}, CreatedAt: time.Unix(1, 0).UTC(),
		Totals:   index.Totals{Files: 1, BytesRaw: 10, BytesStored: int64(len(combined))},
		Archive:  index.ArchiveInfo{Format: "tar", Compression: "store"},
		Chunking: index.ChunkingInfo{Strategy: "length", TargetChunkBytes: 1024, Count: 2},
		Layers:   []index.LayerInfo{{Index: 0, Digest: "sha256:data", ChunkFrom: 0, ChunkTo: 1, StoredBytes: int64(len(combined))}},
		Index:    index.Ref{Path: "index.json.zst"},
	}
	img, err := ociimg.BuildImage(ociimg.BuildOptions{
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, SelfExtract: []byte("fake executable"), Runnable: true,
		Manifest: m, ChunkTable: &index.ChunkTable{SchemaVersion: 1, Chunks: rows}, IndexBlob: []byte("index bytes"), KeyFiles: map[string][]byte{"keys.age": []byte("age key bytes")}, DataLayers: []v1.Layer{data}, Codec: codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	data.compressed.Store(0)
	return img, data, a, b
}

func chunkRow(i int, data []byte) index.Chunk {
	h := sha256.Sum256(data)
	d := "sha256:" + hex.EncodeToString(h[:])
	return index.Chunk{I: i, P: "backup/data/000000.blob", Ps: d, Ss: d, Pb: int64(len(data)), Sb: int64(len(data))}
}

func TestBlobLayerMappingAndPersistentCache(t *testing.T) {
	img, layer, a, b := sourceFixture(t)
	cache := t.TempDir()
	s, err := newImageSource(img, SourceOptions{CacheDir: cache, CacheSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.Manifest(context.Background()); err != nil || m.Tool.Version != "test" {
		t.Fatalf("manifest = %#v, %v", m, err)
	}
	if key, err := s.KeyFile(context.Background(), "keys.age"); err != nil || string(key) != "age key bytes" {
		t.Fatalf("key = %q, %v", key, err)
	}
	if _, err := s.KeyFile(context.Background(), "other"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected key error: %v", err)
	}
	if got, err := s.IndexBlob(context.Background()); err != nil || string(got) != "index bytes" {
		t.Fatalf("index = %q, %v", got, err)
	}
	if got, err := s.Blob(context.Background(), 0); err != nil || !bytes.Equal(got, a) {
		t.Fatalf("blob 0 = %q, %v", got, err)
	}
	if got, err := s.Blob(context.Background(), 1); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("blob 1 = %q, %v", got, err)
	}
	if got := layer.compressed.Load(); got != 1 {
		t.Fatalf("layer downloads = %d, want 1", got)
	}

	s2, err := newImageSource(img, SourceOptions{CacheDir: cache, CacheSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Blob(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := layer.compressed.Load(); got != 1 {
		t.Fatalf("persistent cache redownloaded layer: %d", got)
	}
}

func TestRegistrySourceAndErrorBranches(t *testing.T) {
	img, _, a, _ := sourceFixture(t)
	idx, err := ociimg.BuildIndex([]ociimg.BuiltImage{{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Image: img}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ggcrregistry.New())
	defer srv.Close()
	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://")+"/repo:tag", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	s, err := FromRegistry(context.Background(), ref, nil, SourceOptions{CacheDir: t.TempDir(), CacheSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Blob(context.Background(), 0); err != nil || !bytes.Equal(got, a) {
		t.Fatalf("remote blob = %q, %v", got, err)
	}
	if _, err := sourcePlatform("linux/amd64/variant/extra"); err == nil {
		t.Fatal("invalid platform accepted")
	}
	if _, err := newImageSource(nil, SourceOptions{}); err == nil {
		t.Fatal("nil image accepted")
	}
	if _, err := s.Blob(context.Background(), -1); err == nil {
		t.Fatal("negative chunk accepted")
	}
}

func TestCacheDisabledPruneAndContext(t *testing.T) {
	img, layer, _, _ := sourceFixture(t)
	cache := t.TempDir()
	s, err := newImageSource(img, SourceOptions{CacheDir: cache, CacheSize: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Blob(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Blob(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if layer.compressed.Load() != 2 {
		t.Fatalf("disabled cache downloads = %d", layer.compressed.Load())
	}

	s.cacheSize = 3
	old := filepath.Join(cache, "old")
	keep := filepath.Join(cache, "keep")
	if err := os.WriteFile(old, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("12"), 0o600); err != nil {
		t.Fatal(err)
	}
	ago := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, ago, ago)
	if err := s.prune(keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old cache file not evicted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&contextReader{ctx: ctx, r: bytes.NewReader([]byte("x"))}).Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("context reader = %v", err)
	}
}

func TestMetadataAndMaterializeFailures(t *testing.T) {
	ctx := context.Background()
	fixtureImage, _, _, _ := sourceFixture(t)
	fixtureLayers, err := fixtureImage.Layers()
	if err != nil {
		t.Fatal(err)
	}
	baseLayer := fixtureLayers[0]
	for name, img := range map[string]v1.Image{
		"layers error": &layersImage{Image: empty.Image, err: io.ErrUnexpectedEOF},
		"too few":      empty.Image,
		"open meta": &layersImage{Image: empty.Image, layers: []v1.Layer{
			baseLayer, &failingLayer{Layer: baseLayer, uncompressedErr: io.ErrClosedPipe},
		}},
		"bad tar": &layersImage{Image: empty.Image, layers: []v1.Layer{
			baseLayer, &failingLayer{Layer: baseLayer},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := newImageSource(img, SourceOptions{CacheDir: t.TempDir(), CacheSize: 1024})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Manifest(ctx); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}

	img, _, _, _ := sourceFixture(t)
	s, err := newImageSource(img, SourceOptions{CacheDir: t.TempDir(), CacheSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.materialize(ctx, 99, "backup/data/nope", "store", 1); err == nil {
		t.Fatal("missing layer accepted")
	}
	if _, _, err := s.materialize(ctx, 0, "backup/data/nope", "store", 1); err == nil {
		t.Fatal("missing tar entry accepted")
	}
	if _, _, err := s.materialize(ctx, 0, "backup/data/000000.blob", "store", 1); err == nil {
		t.Fatal("wrong expected size accepted")
	}
	if _, err := s.Blob(ctx, 99); err == nil {
		t.Fatal("out-of-range blob accepted")
	}
	if _, err := s.Manifest(ctx); err != nil {
		t.Fatal(err)
	}
	s.manifest.Layers = nil
	if _, err := s.Blob(ctx, 0); err == nil {
		t.Fatal("unassigned chunk accepted")
	}
}

func TestCorruptCacheIsRedownloaded(t *testing.T) {
	img, layer, a, _ := sourceFixture(t)
	cache := t.TempDir()
	s, err := newImageSource(img, SourceOptions{CacheDir: cache, CacheSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Blob(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache entries = %v, %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(cache, entries[0].Name()), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Blob(context.Background(), 0)
	if err != nil || !bytes.Equal(got, a) {
		t.Fatalf("redownload = %q, %v", got, err)
	}
	if layer.compressed.Load() != 2 {
		t.Fatalf("downloads = %d, want 2", layer.compressed.Load())
	}
}

func TestMissingAndMalformedMetadata(t *testing.T) {
	codec, err := compress.Get("store")
	if err != nil {
		t.Fatal(err)
	}
	makeMeta := func(name string, data []byte) v1.Layer {
		l, err := ociimg.NewLayer([]ociimg.LayerFile{{Path: "/backup/" + name, Mode: 0o644, Size: int64(len(data)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}}, codec, 0)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	fixture, _, _, _ := sourceFixture(t)
	layers, err := fixture.Layers()
	if err != nil {
		t.Fatal(err)
	}
	base := layers[0]
	for name, meta := range map[string]v1.Layer{
		"missing manifest": makeMeta("keys.age", []byte("x")),
		"bad manifest":     makeMeta("manifest.json", []byte("not-json")),
	} {
		t.Run(name, func(t *testing.T) {
			s, err := newImageSource(&layersImage{Image: empty.Image, layers: []v1.Layer{base, meta}}, SourceOptions{CacheDir: t.TempDir(), CacheSize: 1024})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Manifest(context.Background()); err == nil {
				t.Fatal("malformed metadata accepted")
			}
		})
	}
	if _, err := FromOCILayout(filepath.Join(t.TempDir(), "missing"), "x"); err == nil {
		t.Fatal("missing layout accepted")
	}
	idx, err := ociimg.BuildIndex([]ociimg.BuiltImage{{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Image: fixture}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imageForPlatform(idx, v1.Platform{OS: "linux", Architecture: "arm64"}); err == nil {
		t.Fatal("missing platform accepted")
	}
	s, err := newImageSource(fixture, SourceOptions{CacheDir: t.TempDir(), CacheSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.materialize(context.Background(), 0, "backup/data/000000.blob", "missing-codec", int64(len("first stored chunksecond stored chunk"))); err == nil {
		t.Fatal("missing codec accepted")
	}
}

func TestFromOCILayoutSelectsNestedPlatformIndex(t *testing.T) {
	img, _, _, _ := sourceFixture(t)
	idx, err := ociimg.BuildIndex([]ociimg.BuiltImage{{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Image: img}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.AppendIndex(idx); err != nil {
		t.Fatal(err)
	}
	s, err := FromOCILayout(dir, "example.test/repo:tag")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.Manifest(context.Background())
	if err != nil || m.Chunking.Count != 2 {
		t.Fatalf("layout manifest = %#v, %v", m, err)
	}
}
