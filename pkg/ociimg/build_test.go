package ociimg

import (
	"archive/tar"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/index"
)

func sampleManifest() *index.Manifest {
	return &index.Manifest{
		SchemaVersion: 1,
		Tool:          index.ToolInfo{Name: "backimage", Version: "0.4.0"},
		CreatedAt:     time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		Sources:       []string{"/data"},
		Totals:        index.Totals{Files: 42, Dirs: 1, Symlinks: 0, BytesRaw: 12345},
		Archive:       index.ArchiveInfo{Format: "tar", Compression: "store", CompressionLevel: 0},
		Encryption:    index.EncryptionInfo{Enabled: true, KDF: "scrypt-age", AEAD: "aes256-gcm", NonceMode: "random", Recipients: []string{"age1:xxxx"}},
		Chunking:      index.ChunkingInfo{Strategy: "length", TargetChunkBytes: 1 << 20, Count: 2},
		Layers: []index.LayerInfo{
			{Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000", ChunkFrom: 0, ChunkTo: 1},
		},
		Index: index.Ref{Path: "index.json.zst", Encrypted: true},
	}
}

func sampleChunkTable() *index.ChunkTable {
	return &index.ChunkTable{
		SchemaVersion: 1,
		Chunks: []index.Chunk{
			{I: 0, P: "backup/data/000000.blob", Ps: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ss: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Pb: 5, Sb: 5},
		},
	}
}

func sampleOpts(t *testing.T, data []v1.Layer) BuildOptions {
	t.Helper()
	codec, err := compress.ByID(compress.Store)
	if err != nil {
		t.Fatal(err)
	}
	return BuildOptions{
		Platform:    v1.Platform{OS: "linux", Architecture: "amd64"},
		SelfExtract: []byte("ELF-bootstrap"),
		Runnable:    true,
		Manifest:    sampleManifest(),
		ChunkTable:  sampleChunkTable(),
		IndexBlob:   []byte("ztream-index"),
		KeyFiles: map[string][]byte{
			"keys.age":      []byte("age-secret"),
			"keys.pass.age": []byte("scrypt-secret"),
		},
		Codec:      codec,
		Created:    "2024-01-02T03:04:05Z",
		DataLayers: data,
	}
}

func sampleDataLayers(t *testing.T) []v1.Layer {
	t.Helper()
	l0, err := NewLayer([]LayerFile{
		{Path: "/backup/data/000000.blob", Mode: 0o644, Size: 5, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("aaaaa")), nil }},
	}, mustStoreCodec(), 0)
	if err != nil {
		t.Fatal(err)
	}
	l1, err := NewLayer([]LayerFile{
		{Path: "/backup/data/000001.blob", Mode: 0o644, Size: 5, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("bbbbb")), nil }},
	}, mustStoreCodec(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return []v1.Layer{l0, l1}
}

func tarEntries(t *testing.T, l v1.Layer) map[string]*tar.Header {
	t.Helper()
	rc, err := l.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	seen := map[string]*tar.Header{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[hdr.Name] = hdr
	}
	return seen
}

func TestBuildImageLayerLayout(t *testing.T) {
	data := sampleDataLayers(t)
	img, err := BuildImage(sampleOpts(t, data))
	if err != nil {
		t.Fatal(err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2+len(data) {
		t.Fatalf("layers = %d, want %d", len(layers), 2+len(data))
	}
	exe := tarEntries(t, layers[0])
	e, ok := exe["/backimage"]
	if !ok {
		t.Fatal("missing /backimage entry")
	}
	if e.Mode != 0o755 {
		t.Fatalf("/backimage mode = %#o, want 0755", e.Mode)
	}
	meta := tarEntries(t, layers[1])
	for _, want := range []string{"/backup/manifest.json", "/backup/chunks.json", "/backup/index.json.zst", "/backup/keys.age", "/backup/keys.pass.age"} {
		if _, ok := meta[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
}

func TestBuildImageConfig(t *testing.T) {
	img, err := BuildImage(sampleOpts(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Config
	if len(c.Entrypoint) != 1 || c.Entrypoint[0] != "/backimage" {
		t.Errorf("entrypoint = %v", c.Entrypoint)
	}
	if len(c.Cmd) != 1 || c.Cmd[0] != "info" {
		t.Errorf("cmd = %v", c.Cmd)
	}
	if c.WorkingDir != "/" || c.User != "0:0" {
		t.Errorf("workingdir/user = %q %q", c.WorkingDir, c.User)
	}
	if c.Env != nil {
		t.Errorf("env must be nil, got %v", c.Env)
	}
	for k, want := range map[string]string{
		"dev.backimage.schema-version":   "1",
		"dev.backimage.tool-version":     "0.4.0",
		"dev.backimage.encrypted":        "true",
		"dev.backimage.files":            "42",
		"dev.backimage.bytes-raw":        "12345",
		"dev.backimage.sources":          "/data",
		"org.opencontainers.image.title": "backimage backup",
	} {
		if c.Labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, c.Labels[k], want)
		}
	}
}

func TestBuildImageReproducible(t *testing.T) {
	a, err := BuildImage(sampleOpts(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildImage(sampleOpts(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	da, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatalf("digest not reproducible: %s vs %s", da, db)
	}
}

func TestBuildImageNoEntrypoint(t *testing.T) {
	o := sampleOpts(t, nil)
	o.SelfExtract = nil
	if _, err := BuildImage(o); err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("want entrypoint error, got %v", err)
	}
}

func TestBuildImageXzRunnableGuard(t *testing.T) {
	codec, err := compress.ByID(compress.Xz)
	if err != nil {
		t.Fatal(err)
	}
	o := sampleOpts(t, nil)
	o.Codec = codec
	o.Runnable = true
	if _, err := BuildImage(o); err == nil || !strings.Contains(err.Error(), "non-standard") {
		t.Fatalf("want non-standard hint, got %v", err)
	}
	o.Runnable = false
	if _, err := BuildImage(o); err != nil {
		t.Fatalf("pull-only xz must build, got %v", err)
	}
}

func TestBuildIndexTwoPlatforms(t *testing.T) {
	data := sampleDataLayers(t)
	build := func(arch string) v1.Image {
		t.Helper()
		o := sampleOpts(t, data)
		o.Platform = v1.Platform{OS: "linux", Architecture: arch}
		img, err := BuildImage(o)
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	idx, err := BuildIndex([]BuiltImage{
		{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Image: build("amd64")},
		{Platform: v1.Platform{OS: "linux", Architecture: "arm64"}, Image: build("arm64")},
	})
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Manifests) != 2 {
		t.Fatalf("manifests = %d, want 2", len(im.Manifests))
	}
	if im.Manifests[0].Platform == nil || im.Manifests[0].Platform.Architecture != "amd64" {
		t.Errorf("first manifest platform = %+v", im.Manifests[0].Platform)
	}
	if im.Manifests[1].Platform == nil || im.Manifests[1].Platform.Architecture != "arm64" {
		t.Errorf("second manifest platform = %+v", im.Manifests[1].Platform)
		if im.Manifests[0].Platform.Architecture == "arm64" && im.Manifests[1].Platform.Architecture == "amd64" {
			t.Logf("(platforms swapped: sorted order is fine)")
		}
	}
}

func TestBuildIndexDataLayerMismatch(t *testing.T) {
	o := sampleOpts(t, sampleDataLayers(t))
	img, err := BuildImage(o)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewLayer([]LayerFile{
		{Path: "/backup/data/000000.blob", Mode: 0o644, Size: 5, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("AAAAA")), nil }},
	}, mustStoreCodec(), 0)
	if err != nil {
		t.Fatal(err)
	}
	o2 := o
	o2.DataLayers = []v1.Layer{other}
	o2.Platform = v1.Platform{OS: "linux", Architecture: "arm64"}
	img2, err := BuildImage(o2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildIndex([]BuiltImage{
		{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Image: img},
		{Platform: v1.Platform{OS: "linux", Architecture: "arm64"}, Image: img2},
	})
	if err == nil || !strings.Contains(err.Error(), "identical") {
		t.Fatalf("want shared-layer error, got %v", err)
	}
}

var _ = types.OCIManifestSchema1

func TestBuildImageNilInputs(t *testing.T) {
	o := sampleOpts(t, nil)
	o.Manifest = nil
	o.ChunkTable = nil
	o.KeyFiles = nil
	if _, err := BuildImage(o); err != nil {
		t.Fatalf("nil manifest/keys must build: %v", err)
	}
	o.IndexBlob = nil
	if _, err := BuildImage(o); err == nil {
		t.Fatal("want error for empty metadata layer")
	}
}

func TestBuildImageDefaultCreated(t *testing.T) {
	o := sampleOpts(t, nil)
	o.Created = ""
	if _, err := BuildImage(o); err != nil {
		t.Fatal(err)
	}
}

func TestNewLayerNegativeLevel(t *testing.T) {
	codec := mustGzipCodec(t)
	l, err := NewLayer(sampleFiles(), codec, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Digest(); err != nil {
		t.Fatal(err)
	}
}
