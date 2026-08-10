package server

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fpierri/backimage/pkg/archive"
	"github.com/fpierri/backimage/pkg/crypt"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/protocol"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type streamSink struct {
	*memorySink
	mu      sync.Mutex
	commit  StreamCommit
	commits int
	failAt  string
}

func newStreamSink() *streamSink { return &streamSink{memorySink: newMemorySink()} }

func (s *streamSink) CommitStream(_ context.Context, commit StreamCommit) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAt == "publish" {
		return "", errors.New("publish failure")
	}
	s.commits++
	s.commit = commit
	return "sha256:published", nil
}

func (s *streamSink) OpenBlob(ctx context.Context, ref, digest string, size int64) (BlobWriter, error) {
	if s.failAt == "open" {
		return nil, errors.New("registry upload start failed")
	}
	writer, err := s.memorySink.OpenBlob(ctx, ref, digest, size)
	if err != nil {
		return nil, err
	}
	if s.failAt == "commit" || s.failAt == "write" {
		return &brokenBlobWriter{BlobWriter: writer, mode: s.failAt}, nil
	}
	return writer, nil
}

type brokenBlobWriter struct {
	BlobWriter
	mode string
}

func (w *brokenBlobWriter) Write(p []byte) (int, error) {
	if w.mode == "write" {
		return 0, errors.New("registry upload interrupted")
	}
	return w.BlobWriter.Write(p)
}

func (w *brokenBlobWriter) Commit(ctx context.Context) error {
	if w.mode == "commit" {
		return errors.New("registry upload commit refused")
	}
	return w.BlobWriter.Commit(ctx)
}

func (s *streamSink) result() StreamCommit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commit
}

// testArchive builds a real tar stream, the same bytes a streaming client
// would put on the wire.
func testArchive(t *testing.T, size int) ([]byte, int64) {
	t.Helper()
	tree := t.TempDir()
	// Incompressible payload: layer boundaries are measured on stored bytes.
	payload := make([]byte, size)
	source := rand.New(rand.NewSource(7))
	if _, err := source.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "data.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "small.txt"), []byte("hello stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writer := archive.NewWriter(&buf, archive.Options{Strict: true})
	if err := writer.AddRoot(context.Background(), tree); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), int64(len(payload))
}

func streamStartFor(t *testing.T, raw int, encrypt bool) (*protocol.StreamStart, *crypt.KeyMaterial) {
	t.Helper()
	start := &protocol.StreamStart{
		Reference:      "registry.test/me/repo:t",
		ToolVersion:    "test",
		Archive:        &protocol.ArchiveConfig{Compression: "zstd"},
		Platforms:      []*protocol.Platform{{Os: "linux", Architecture: "amd64"}},
		EstimatedBytes: uint64(raw),
		MaxLayerBytes:  8 << 20,
		Encryption:     &protocol.EncryptionConfig{},
	}
	if !encrypt {
		return start, nil
	}
	km, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	start.Encryption = &protocol.EncryptionConfig{
		Enabled: true, Dek: km.DEK, NonceKey: km.NonceKey, NonceMode: "random",
		KeyFiles: map[string][]byte{"keys.pass.age": []byte("wrapped")},
	}
	return start, km
}

func runIngest(t *testing.T, sink Sink, start *protocol.StreamStart, stream []byte) (StreamResult, error) {
	t.Helper()
	in, err := startIngest(context.Background(), ingestConfig{
		Start: start, SessionID: "session", Reference: start.Reference,
		TempDir: t.TempDir(), Sink: sink,
	})
	if err != nil {
		return StreamResult{}, err
	}
	for offset := 0; offset < len(stream); offset += 1 << 20 {
		end := min(offset+(1<<20), len(stream))
		if writeErr := in.Write(stream[offset:end]); writeErr != nil {
			// The pipeline already failed: Finish reports the real cause.
			break
		}
	}
	return in.Finish()
}

func TestIngestBuildsLayersManifestAndIndex(t *testing.T) {
	for _, encrypt := range []bool{false, true} {
		name := map[bool]string{false: "plain", true: "encrypted"}[encrypt]
		t.Run(name, func(t *testing.T) {
			stream, rawPayload := testArchive(t, 24<<20)
			sink := newStreamSink()
			start, km := streamStartFor(t, len(stream), encrypt)
			if km != nil {
				defer km.Wipe()
			}
			result, err := runIngest(t, sink, start, stream)
			if err != nil {
				t.Fatal(err)
			}
			if result.Digest != "sha256:published" {
				t.Fatalf("digest = %q", result.Digest)
			}
			if result.Layers < 2 {
				t.Fatalf("layers = %d, want the 8 MiB target to split the stream", result.Layers)
			}
			if result.Chunks == 0 || result.StoredBytes == 0 || result.UploadedBytes == 0 {
				t.Fatalf("result = %+v", result)
			}
			if result.RawBytes < uint64(rawPayload) {
				t.Fatalf("raw bytes = %d, want >= %d", result.RawBytes, rawPayload)
			}
			if result.Files != 2 {
				t.Fatalf("files = %d, want 2", result.Files)
			}

			commit := sink.result()
			if commit.Manifest == nil || commit.ChunkTable == nil || len(commit.IndexBlob) == 0 {
				t.Fatal("commit is missing manifest, chunk table or index blob")
			}
			if got := len(commit.Layers); got != int(result.Layers) {
				t.Fatalf("committed layers = %d, want %d", got, result.Layers)
			}
			if commit.Manifest.Encryption.Enabled != encrypt {
				t.Fatalf("manifest encryption = %+v", commit.Manifest.Encryption)
			}
			if commit.Manifest.Totals.Files != 2 || commit.Manifest.Totals.BytesRaw < int64(rawPayload) {
				t.Fatalf("manifest totals = %+v", commit.Manifest.Totals)
			}
			// Chunk rows must cover every layer contiguously and name the
			// content-addressed blob they live in.
			var stored int64
			for i, row := range commit.ChunkTable.Chunks {
				if row.I != i {
					t.Fatalf("chunk %d has index %d", i, row.I)
				}
				if !strings.HasPrefix(row.P, "backup/data/") || !strings.HasSuffix(row.P, ".blob") {
					t.Fatalf("chunk %d path = %q", i, row.P)
				}
				stored += row.Sb
			}
			if uint64(stored) != result.StoredBytes {
				t.Fatalf("chunk table stores %d bytes, result says %d", stored, result.StoredBytes)
			}
			for i, layer := range commit.Manifest.Layers {
				if layer.ChunkFrom > layer.ChunkTo || layer.StoredBytes == 0 {
					t.Fatalf("layer %d = %+v", i, layer)
				}
				if i > 0 && layer.ChunkFrom != commit.Manifest.Layers[i-1].ChunkTo+1 {
					t.Fatalf("layer %d does not continue the previous one: %+v", i, layer)
				}
			}
			// Every data layer must be in the registry, byte for byte.
			for _, layer := range commit.Layers {
				sink.mu.Lock()
				_, ok := sink.memorySink.blobs[layer.Digest]
				sink.mu.Unlock()
				if !ok {
					t.Fatalf("layer %s was never uploaded", layer.Digest)
				}
			}
			// The index must be readable with the key the client provided.
			var opener crypt.Opener
			if km != nil {
				var openErr error
				opener, openErr = crypt.NewOpener(km)
				if openErr != nil {
					t.Fatal(openErr)
				}
			}
			decoded, err := index.ReadIndex(bytes.NewReader(commit.IndexBlob), opener)
			if err != nil {
				t.Fatalf("index blob: %v", err)
			}
			if len(decoded.Entries) != 3 {
				t.Fatalf("index entries = %d, want 3 (root dir + 2 files)", len(decoded.Entries))
			}
		})
	}
}

func TestIngestRejectsSinkWithoutStreamSupport(t *testing.T) {
	start, _ := streamStartFor(t, 1024, false)
	if _, err := startIngest(context.Background(), ingestConfig{
		Start: start, Reference: start.Reference, TempDir: t.TempDir(), Sink: newMemorySink(),
	}); err == nil {
		t.Fatal("a sink that cannot publish streams was accepted")
	}
}

func TestIngestPropagatesUploadFailureAndCleansUp(t *testing.T) {
	stream, _ := testArchive(t, 12<<20)
	sink := newStreamSink()
	sink.failAt = "open"
	start, _ := streamStartFor(t, len(stream), false)
	temp := t.TempDir()
	in, err := startIngest(context.Background(), ingestConfig{
		Start: start, Reference: start.Reference, TempDir: temp, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(stream); offset += 1 << 20 {
		end := min(offset+(1<<20), len(stream))
		if writeErr := in.Write(stream[offset:end]); writeErr != nil {
			break
		}
	}
	if _, err := in.Finish(); err == nil {
		t.Fatal("upload failure was not reported")
	}
	assertNoSpool(t, temp)
}

func TestIngestAbortReleasesSpool(t *testing.T) {
	stream, _ := testArchive(t, 12<<20)
	start, _ := streamStartFor(t, len(stream), false)
	temp := t.TempDir()
	in, err := startIngest(context.Background(), ingestConfig{
		Start: start, Reference: start.Reference, TempDir: temp, Sink: newStreamSink(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Write(stream[:1<<20]); err != nil {
		t.Fatal(err)
	}
	in.Abort()
	assertNoSpool(t, temp)
}

func TestIngestDedupSkipsLayersAlreadyInTheRegistry(t *testing.T) {
	stream, _ := testArchive(t, 40<<20)
	sink := newStreamSink()
	start, km := streamStartFor(t, len(stream), true)
	defer km.Wipe()
	start.Dedup = true
	start.Encryption.NonceMode = "convergent"
	start.Cdc = &protocol.CDCParams{Min: 1 << 20, Avg: 2 << 20, Max: 8 << 20}

	first, err := runIngest(t, sink, start, stream)
	if err != nil {
		t.Fatal(err)
	}
	if first.LayersSkipped != 0 || first.Layers < 2 {
		t.Fatalf("first run = %+v", first)
	}
	commit := sink.result()
	if commit.Manifest.Chunking.Strategy != "cdc" || commit.Manifest.Chunking.Polynomial == 0 {
		t.Fatalf("chunking = %+v", commit.Manifest.Chunking)
	}

	// A second identical stream must find every layer already present.
	second, err := runIngest(t, sink, start, stream)
	if err != nil {
		t.Fatal(err)
	}
	if second.LayersSkipped != second.Layers || second.UploadedBytes != 0 {
		t.Fatalf("second run = %+v, want every layer skipped", second)
	}
}

func TestIngestReportsFailureToTheWriter(t *testing.T) {
	// A large stream rolls several layers, so the upload failure surfaces
	// while the client is still writing.
	stream, _ := testArchive(t, 40<<20)
	sink := newStreamSink()
	sink.failAt = "open"
	start, _ := streamStartFor(t, len(stream), false)
	in, err := startIngest(context.Background(), ingestConfig{
		Start: start, Reference: start.Reference, TempDir: t.TempDir(), Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	var writeErr error
	for offset := 0; offset < len(stream); offset += 1 << 20 {
		end := min(offset+(1<<20), len(stream))
		if writeErr = in.Write(stream[offset:end]); writeErr != nil {
			break
		}
	}
	if writeErr == nil {
		t.Fatal("the writer never saw the pipeline failure")
	}
	if !strings.Contains(writeErr.Error(), "registry") {
		t.Fatalf("write error = %v, want the pipeline cause", writeErr)
	}
	if _, err := in.Finish(); err == nil {
		t.Fatal("Finish must report the failure too")
	}
}

func TestIngestRejectsInvalidStreamStart(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*protocol.StreamStart)
		wantErr string
	}{
		{"unknown codec", func(s *protocol.StreamStart) { s.Archive = &protocol.ArchiveConfig{Compression: "brotli"} }, "brotli"},
		{"bad created", func(s *protocol.StreamStart) { s.Created = "yesterday" }, "created"},
		{"bad cdc", func(s *protocol.StreamStart) {
			s.Dedup = true
			s.Cdc = &protocol.CDCParams{Min: 8 << 20, Avg: 1 << 20, Max: 2 << 20}
		}, "CDC"},
		{"short key", func(s *protocol.StreamStart) {
			s.Encryption = &protocol.EncryptionConfig{Enabled: true, Dek: []byte("too short")}
		}, "key material"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, _ := streamStartFor(t, 1<<20, false)
			tc.mutate(start)
			_, err := startIngest(context.Background(), ingestConfig{
				Start: start, Reference: start.Reference, TempDir: t.TempDir(), Sink: newStreamSink(),
			})
			if err == nil {
				t.Fatal("invalid StreamStart accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestIngestReportsPublishFailure(t *testing.T) {
	stream, _ := testArchive(t, 4<<20)
	sink := newStreamSink()
	sink.failAt = "publish"
	start, _ := streamStartFor(t, len(stream), false)
	// An empty codec name must fall back to the default rather than fail.
	start.Archive = &protocol.ArchiveConfig{}
	if _, err := runIngest(t, sink, start, stream); err == nil {
		t.Fatal("publish failure was not reported")
	}
}

func TestIngestReportsBlobWriteAndCommitFailures(t *testing.T) {
	for _, mode := range []string{"write", "commit"} {
		t.Run(mode, func(t *testing.T) {
			stream, _ := testArchive(t, 4<<20)
			sink := newStreamSink()
			sink.failAt = mode
			start, _ := streamStartFor(t, len(stream), false)
			temp := t.TempDir()
			in, err := startIngest(context.Background(), ingestConfig{
				Start: start, Reference: start.Reference, TempDir: temp, Sink: sink,
			})
			if err != nil {
				t.Fatal(err)
			}
			for offset := 0; offset < len(stream); offset += 1 << 20 {
				end := min(offset+(1<<20), len(stream))
				if writeErr := in.Write(stream[offset:end]); writeErr != nil {
					break
				}
			}
			if _, err := in.Finish(); err == nil {
				t.Fatalf("%s failure was not reported", mode)
			}
			if mode == "write" && sink.memorySink.aborted == 0 {
				t.Fatal("a failed upload must release the registry session")
			}
			assertNoSpool(t, temp)
		})
	}
}

func TestIngestStopsOnCanceledContext(t *testing.T) {
	stream, _ := testArchive(t, 4<<20)
	start, _ := streamStartFor(t, len(stream), false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in, err := startIngest(ctx, ingestConfig{
		Start: start, Reference: start.Reference, TempDir: t.TempDir(), Sink: newStreamSink(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(stream); offset += 1 << 20 {
		end := min(offset+(1<<20), len(stream))
		if writeErr := in.Write(stream[offset:end]); writeErr != nil {
			break
		}
	}
	if _, err := in.Finish(); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestIngestRejectsEmptyStream(t *testing.T) {
	start, _ := streamStartFor(t, 0, false)
	if _, err := runIngest(t, newStreamSink(), start, nil); err == nil {
		t.Fatal("an empty stream was accepted")
	}
}

func TestStreamHelpers(t *testing.T) {
	if got := safeLayerMaximum(0); got != int64(^uint64(0)>>1) {
		t.Fatalf("safeLayerMaximum(0) = %d", got)
	}
	if got := safeLayerMaximum(1 << 20); got != 4<<20 {
		t.Fatalf("safeLayerMaximum(1MiB) = %d", got)
	}
	if got := safeLayerMaximum(int64(^uint64(0)>>1) / 2); got != int64(^uint64(0)>>1) {
		t.Fatalf("safeLayerMaximum(huge) = %d", got)
	}
	if got := dataBlobPath("sha256:abc"); got != "backup/data/abc.blob" {
		t.Fatalf("dataBlobPath = %q", got)
	}
	if got := compressionName(&protocol.StreamStart{}); got != "zstd" {
		t.Fatalf("default compression = %q", got)
	}

	// Close to the OCI budget the content-defined boundaries give way to
	// fixed-size ones so the layer count stays bounded.
	builder := &streamBuilder{
		cfg:    ingestConfig{Start: &protocol.StreamStart{Dedup: true, EstimatedBytes: 1 << 30}},
		stats:  new(streamStats),
		layers: make([]Layer, 110),
	}
	builder.applyBoundaryFallback()
	if !builder.boundaryFallback || builder.boundary == nil {
		t.Fatal("the boundary fallback did not engage at 110 layers")
	}
	builder.boundaryFallback = false
	builder.layers = make([]Layer, 3)
	builder.applyBoundaryFallback()
	if builder.boundaryFallback {
		t.Fatal("the fallback engaged too early")
	}
}

// brokenLayer fails at whichever descriptor field the test selects.
type brokenLayer struct {
	v1.Layer
	failOn string
}

func (l brokenLayer) Digest() (v1.Hash, error) {
	if l.failOn == "digest" {
		return v1.Hash{}, errors.New("digest failure")
	}
	return v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("a", 64)}, nil
}

func (l brokenLayer) DiffID() (v1.Hash, error) {
	if l.failOn == "diffid" {
		return v1.Hash{}, errors.New("diff id failure")
	}
	return v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("b", 64)}, nil
}

func (l brokenLayer) Size() (int64, error) {
	if l.failOn == "size" {
		return 0, errors.New("size failure")
	}
	return 1, nil
}

func (l brokenLayer) MediaType() (types.MediaType, error) {
	if l.failOn == "mediatype" {
		return "", errors.New("media type failure")
	}
	return types.OCILayer, nil
}

func TestLayerDescriptorPropagatesFailures(t *testing.T) {
	for _, failOn := range []string{"digest", "diffid", "size", "mediatype"} {
		if _, err := layerDescriptor(brokenLayer{failOn: failOn}, 0); err == nil {
			t.Fatalf("%s failure was swallowed", failOn)
		}
	}
	descriptor, err := layerDescriptor(brokenLayer{}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Index != 7 || descriptor.Size != 1 || descriptor.MediaType != string(types.OCILayer) {
		t.Fatalf("descriptor = %+v", descriptor)
	}
}

func TestNewSpoolRejectsUnusableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newSpool(file, 0); err == nil {
		t.Fatal("a spool directory that is a file was accepted")
	}
}

func TestSessionErrorAndSinkAccessors(t *testing.T) {
	if got := (&SessionError{Kind: ErrorUsage, Message: "boom"}).Error(); got != "boom" {
		t.Fatalf("SessionError = %q", got)
	}
	sink, err := NewRegistrySink(RegistrySinkOptions{Broker: NewTokenBroker(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	// A stateless server cannot resume by session ID: it must say so instead of
	// guessing.
	known, err := sink.KnownBlobs(context.Background(), "session")
	if err != nil || known != nil {
		t.Fatalf("known blobs = %v, %v", known, err)
	}
	sink.ProvideToken(&protocol.Token{Value: "t", Repository: "me/repo", Actions: []string{"pull"}})
	if _, err := sink.selfExtract(&protocol.BackupStart{SelfextractAmd64: []byte("elf")}, "amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.selfExtract(&protocol.BackupStart{}, "riscv64"); err == nil {
		t.Fatal("an unsupported architecture was accepted")
	}
	if _, err := sink.selfExtract(&protocol.BackupStart{}, "arm64"); err == nil {
		t.Fatal("a missing self-extract binary was accepted")
	}
}

func assertNoSpool(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "backimage-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}
