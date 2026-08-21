package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/ociimg"
)

// verifyFixture builds a one-layer image whose manifest carries the real layer
// digest, so the streaming verification has something truthful to compare
// against. tamper mutates the chunk table or the manifest before the image is
// assembled, which is how a corruption is simulated.
func verifyFixture(t *testing.T, tamper func(m *index.Manifest, table *index.ChunkTable)) v1.Image {
	t.Helper()
	codec, err := compress.Get("store")
	if err != nil {
		t.Fatal(err)
	}
	a, b := []byte("first stored chunk"), []byte("second stored chunk")
	combined := append(append([]byte(nil), a...), b...)
	data, err := ociimg.NewLayer([]ociimg.LayerFile{{
		Path: "/backup/data/000000.blob", Mode: 0o644, Size: int64(len(combined)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(combined)), nil },
	}}, codec, 0)
	if err != nil {
		t.Fatal(err)
	}
	// LayerInfo.Digest is the digest of the blob inside the layer, not of the
	// compressed layer: that is what the pipeline records when it rolls one.
	blobSum := sha256.Sum256(combined)
	blobDigest := "sha256:" + hex.EncodeToString(blobSum[:])
	table := &index.ChunkTable{SchemaVersion: 1, Chunks: []index.Chunk{chunkRow(0, a), chunkRow(1, b)}}
	m := &index.Manifest{
		SchemaVersion: 1, Tool: index.ToolInfo{Name: "backimage", Version: "test"}, CreatedAt: time.Unix(1, 0).UTC(),
		Totals:   index.Totals{Files: 1, BytesRaw: 10, BytesStored: int64(len(combined))},
		Archive:  index.ArchiveInfo{Format: "tar", Compression: "store"},
		Chunking: index.ChunkingInfo{Strategy: "length", TargetChunkBytes: 1024, Count: 2},
		Layers: []index.LayerInfo{{
			Index: 0, Digest: blobDigest, ChunkFrom: 0, ChunkTo: 1, StoredBytes: int64(len(combined)),
		}},
		Index: index.Ref{Path: "index.json.zst"},
	}
	if tamper != nil {
		tamper(m, table)
	}
	img, err := ociimg.BuildImage(ociimg.BuildOptions{
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, SelfExtract: []byte("fake executable"), Runnable: true,
		Manifest: m, ChunkTable: table, IndexBlob: []byte("index bytes"),
		KeyFiles: map[string][]byte{"keys.age": []byte("age key bytes")}, DataLayers: []v1.Layer{data}, Codec: codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestVerifyStoredStreamingHappyPath(t *testing.T) {
	// A cache directory that must never come into existence: the verification
	// is one streaming pass and writes nothing to disk.
	cache := filepath.Join(t.TempDir(), "layers")
	src, err := newImageSource(verifyFixture(t, nil), SourceOptions{CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyStoredSource(context.Background(), src, false, nil)
	if err != nil {
		t.Fatalf("verification of an intact image must pass: %v", err)
	}
	if !report.OK || report.Layers != 1 || report.Chunks != 2 {
		t.Fatalf("report = %+v, want 1 layer and 2 chunks", report)
	}
	if report.Bytes != int64(len("first stored chunk")+len("second stored chunk")) {
		t.Fatalf("bytes re-read = %d", report.Bytes)
	}
	// The layer cache must stay untouched: the verification is a streaming
	// pass, so it must not write anything to disk.
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("the streaming verification must not create the layer cache")
	}
}

func TestVerifyStoredDetectsChunkCorruption(t *testing.T) {
	// The stored digest of the second chunk no longer describes its bytes.
	img := verifyFixture(t, func(_ *index.Manifest, table *index.ChunkTable) {
		sum := sha256.Sum256([]byte("something else entirely"))
		table.Chunks[1].Ss = "sha256:" + hex.EncodeToString(sum[:])
	})
	src, err := newImageSource(img, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyStoredSource(context.Background(), src, true, nil)
	if !errors.Is(err, ErrStoredMismatch) {
		t.Fatalf("a corrupted chunk must fail the verification, got %v", err)
	}
	if report.OK || len(report.Errors) == 0 {
		t.Fatalf("report = %+v, want the mismatch recorded", report)
	}
	if !strings.Contains(report.Errors[0], "chunk 1") {
		t.Fatalf("the error must name the chunk: %q", report.Errors[0])
	}
	if report.Chunks != 1 {
		t.Fatalf("chunks verified = %d, want the intact one only", report.Chunks)
	}
}

func TestVerifyStoredDetectsBlobDigestMismatch(t *testing.T) {
	img := verifyFixture(t, func(m *index.Manifest, _ *index.ChunkTable) {
		m.Layers[0].Digest = "sha256:" + strings.Repeat("00", 32)
	})
	src, err := newImageSource(img, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyStoredSource(context.Background(), src, true, nil)
	if !errors.Is(err, ErrStoredMismatch) {
		t.Fatalf("a layer digest mismatch must fail, got %v", err)
	}
	joined := strings.Join(report.Errors, "\n")
	if !strings.Contains(joined, "digest del blob") {
		t.Fatalf("errors must explain the digest disagreement: %q", joined)
	}
}

type plainSource struct{ Source }

func TestVerifyStoredUnsupportedSource(t *testing.T) {
	if _, err := VerifyStoredSource(context.Background(), plainSource{}, false, nil); !errors.Is(err, ErrStoredVerifyUnsupported) {
		t.Fatalf("a source without streaming support must say so, got %v", err)
	}
}
