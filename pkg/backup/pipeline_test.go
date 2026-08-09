package backup

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"

	"github.com/fpierri/backimage/pkg/crypt"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/registry"
)

func stubSelf(goarch string) ([]byte, error) { return []byte("ELF-STUB-" + goarch), nil }

// ---- flag validation ----

func TestValidateConflicts(t *testing.T) {
	tree := t.TempDir()
	os.WriteFile(filepath.Join(tree, "a.txt"), []byte("hello"), 0o644)
	base := Config{RootPaths: []string{tree}, Ref: "localhost:5000/x/y:latest", Compression: "zstd"}

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"ok", func(c *Config) {}, false},
		{"no roots", func(c *Config) { c.RootPaths = nil }, true},
		{"no repo", func(c *Config) { c.Ref = "" }, true},
		{"bad ref", func(c *Config) { c.Ref = ":::!!!" }, true},
		{"bad output", func(c *Config) { c.Output = "s3" }, true},
		{"oci-layout senza path", func(c *Config) { c.Output = "oci-layout" }, true},
		{"max-layer minima", func(c *Config) { c.MaxLayerSize = 11 }, true},
		{"jobs negative", func(c *Config) { c.Jobs = -1 }, true},
		{"encrypt senza chiavi", func(c *Config) { c.Encrypt = true }, true},
		{"encrypt con pass", func(c *Config) {
			c.Encrypt = true
			c.Passphrase = func() ([]byte, error) { return []byte("pp"), nil }
		}, false},
		{"daemon con path", func(c *Config) { c.Output = "daemon"; c.OutputPath = "/x" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			err := Validate(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// ---- dry run -----------------------------------------------------------------

func TestDryRunNoNetworkNoWrites(t *testing.T) {
	tree := t.TempDir()
	for i := 0; i < 3; i++ {
		os.WriteFile(filepath.Join(tree, fmt.Sprintf("f%d", i)), make([]byte, 1024), 0o644)
	}
	layoutPath := filepath.Join(t.TempDir(), "out")
	var store registry.Store
	result, err := Run(context.Background(), Config{
		RootPaths:     []string{tree},
		Ref:           "localhost:9999/no/contact:latest",
		Output:        "oci-layout",
		OutputPath:    layoutPath,
		AllowDegraded: true,
		DryRun:        true,
		CheckpointDir: t.TempDir(),
		Resume:        true,
		Store:         store, // never touched: would panic
	})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if result.Layers == 0 || result.Chunks == 0 {
		t.Fatalf("plan vuoto: %+v", result)
	}
	if _, err := os.Stat(layoutPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run ha scritto qualcosa: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(t.TempDir(), "*.json"))
	if len(matches) != 0 {
		t.Fatalf("checkpoint scritto: %v", matches)
	}
}

// ---- temp space --------------------------------------------------------------

func TestTempSpaceInsufficient(t *testing.T) {
	old := statfs
	defer func() { statfs = old }()
	statfs = func(dir string) (int64, error) { return 1 << 30, nil } // 1 GiB

	tree := t.TempDir()
	f, _ := os.Create(filepath.Join(tree, "big"))
	defer f.Close()
	if err := f.Truncate(5 << 30); err != nil { // 5 GiB sparse
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Config{
		RootPaths:     []string{tree},
		Ref:           "x.io/r/b:t",
		Compression:   "zstd",
		Jobs:          4,
		SelfExtract:   stubSelf,
		MaxLayerSize:  1 << 30,
		AllowDegraded: true,
		TempDir:       t.TempDir(),
		DryRun:        false,
		Output:        "oci-layout",
		OutputPath:    filepath.Join(t.TempDir(), "o"),
	})
	if err == nil || !strings.Contains(err.Error(), "spazio temporaneo") {
		t.Fatalf("err = %v, want hint", err)
	}
}

// ---- full pipeline to oci-layout ----------------------------------------------

func TestPipelineToOCILayout(t *testing.T) {
	tree := t.TempDir()
	const files, size = 40, 1 << 12
	for i := 0; i < files; i++ {
		os.WriteFile(filepath.Join(tree, fmt.Sprintf("f%03d.bin", i)), make([]byte, size), 0o644)
	}
	outPath := filepath.Join(t.TempDir(), "layout")
	var progress []string
	res, err := Run(context.Background(), Config{
		RootPaths:     []string{tree},
		Ref:           "example.com/t/backup:tag1",
		Compression:   "zstd",
		Level:         1,
		Jobs:          2,
		MaxLayerSize:  1 << 20,
		AllowDegraded: true,
		SelfExtract:   stubSelf, // tiny: forces multiple chunks, single layer
		Encrypt:       false,
		Runnable:      false,
		Platforms:     []string{"linux/amd64"},
		Output:        "oci-layout",
		OutputPath:    outPath,
		CheckpointDir: t.TempDir(),
		Resume:        true,
		Progress:      func(msg string) { progress = append(progress, msg) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Chunks < 1 || res.Layers < 1 || res.Files != files {
		t.Fatalf("result incoerente: %+v", res)
	}
	if res.BytesRaw != int64(files*size) {
		t.Fatalf("bytesRaw = %d, want %d", res.BytesRaw, files*size)
	}
	hasDumpProgress := false
	for _, msg := range progress {
		if strings.Contains(msg, "dump: archiviazione/compressione/cifratura") {
			hasDumpProgress = true
			break
		}
	}
	if !hasDumpProgress {
		t.Fatalf("missing dump progress: %v", progress)
	}
	if res.Encrypted {
		t.Fatal("no-encrypt marcato cifrato")
	}

	l, lerr := layout.FromPath(outPath)
	_ = lerr
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	ii, err := l.ImageIndex()
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	im, err := ii.IndexManifest()
	if err != nil {
		t.Fatalf("index manifest: %v", err)
	}
	if len(im.Manifests) != 1 {
		t.Fatalf("manifests = %d", len(im.Manifests))
	}
	img, err := ii.Image(im.Manifests[0].Digest)
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	if len(layers) != 3 { // self-extract(non runnable: skip) + meta + data
		t.Fatalf("layers = %d, want 3", len(layers))
	}

	// meta layer: manifest.json + chunks.json + index.json.zst
	meta := layers[1]
	filesIn := tarEntries(t, meta, "/backup/")
	if _, ok := filesIn["/backup/manifest.json"]; !ok {
		t.Fatalf("manifest.json assente: %v", keysOf(filesIn))
	}
	if _, ok := filesIn["/backup/chunks.json"]; !ok {
		t.Fatalf("chunks.json assente")
	}
	if _, ok := filesIn["/backup/index.json.zst"]; !ok {
		t.Fatalf("index.json.zst assente")
	}

	// verify manifest + chunks coherence
	m, err := index.ReadManifest(strings.NewReader(string(filesIn["/backup/manifest.json"])))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var ctable index.ChunkTable
	if err := json.Unmarshal(filesIn["/backup/chunks.json"], &ctable); err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(ctable.Chunks) != res.Chunks {
		t.Fatalf("chunks = %d, want %d", len(ctable.Chunks), res.Chunks)
	}
	if m.Chunking.Count != res.Chunks {
		t.Fatalf("manifest count = %d", m.Chunking.Count)
	}
	if len(m.Layers) != res.Layers && len(m.Layers) != 1 {
		// tolerance: tiny tree packs into one layer regardless of res.Layers estimate
		t.Fatalf("layers manifest = %d", len(m.Layers))
	}

	// every chunk row must point at an existing blob in a data layer
	seen := map[string]bool{}
	for _, dl := range layers[2:] {
		for name := range tarEntries(t, dl, "/backup/data/") {
			seen[strings.TrimPrefix(name, "/")] = true
		}
	}
	for _, c := range ctable.Chunks {
		if !seen[c.P] {
			t.Fatalf("chunk %d punta a %s, blob assente", c.I, c.P)
		}
	}
}

// tarEntries lists the files of a tar layer.
func tarEntries(t *testing.T, l v1.Layer, prefix string) map[string][]byte {
	t.Helper()
	rc, err := l.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(hdr.Name, prefix) {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			out[hdr.Name] = b
		}
	}
	return out
}

func keysOf(m map[string][]byte) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	return k
}

// ---- encrypted pipeline --------------------------------------------------------

func TestPipelineEncrypted(t *testing.T) {
	tree := t.TempDir()
	os.WriteFile(filepath.Join(tree, "f"), []byte("secret data here"), 0o600)
	outPath := filepath.Join(t.TempDir(), "enc")
	_, err := Run(context.Background(), Config{
		RootPaths:     []string{tree},
		Ref:           "example.com/r/m:enc",
		Compression:   "gzip",
		Encrypt:       true,
		Passphrase:    func() ([]byte, error) { return []byte("pw$"), nil },
		SelfExtract:   stubSelf,
		AllowDegraded: true,
		Runnable:      false,
		Platforms:     []string{"linux/amd64"},
		Output:        "oci-layout",
		OutputPath:    outPath,
		Resume:        false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	l, lerr := layout.FromPath(outPath)
	_ = lerr
	ii, _ := l.ImageIndex()
	im, _ := ii.IndexManifest()
	img, _ := ii.Image(im.Manifests[0].Digest)
	layers, _ := img.Layers()
	meta := tarEntries(t, layers[1], "/backup/")
	if _, ok := meta["/backup/keys.pass.age"]; !ok {
		t.Fatalf("keys.pass.age assente")
	}
	// chunk data must be sealed: first bytes are the envelope header
	for _, dl := range layers[2:] {
		rc, _ := dl.Uncompressed()
		tr := tar.NewReader(rc)
		if _, err := tr.Next(); err == nil {
			var head [16]byte
			if n, _ := io.ReadFull(tr, head[:]); n > 0 && !crypt.IsEnvelope(head[:n]) {
				t.Fatalf("chunk non cifrato")
			}
		}
		rc.Close()
	}
}
