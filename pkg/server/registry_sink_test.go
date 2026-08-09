package server

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fpierri/backimage/pkg/compress"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/ociimg"
	"github.com/fpierri/backimage/pkg/protocol"
	"github.com/google/go-containerregistry/pkg/name"
	gcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestRegistrySinkStreamsAndPublishesIndex(t *testing.T) {
	srv := httptest.NewServer(gcrregistry.New())
	defer srv.Close()
	reference := strings.TrimPrefix(srv.URL, "http://") + "/e2e/remote:t1"
	ref, err := name.ParseReference(reference, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	broker := NewTokenBroker(time.Second)
	broker.ProvideToken(&protocol.Token{
		Value: "memory-only", Repository: ref.Context().RepositoryStr(), Actions: []string{"pull", "push"},
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	})
	sink, err := NewRegistrySink(RegistrySinkOptions{
		Broker: broker, ChunkSize: 32 << 20,
		SelfExtract: func(string) ([]byte, error) { return []byte("selfextract"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	repository, actions, err := sink.TokenScope(reference)
	if err != nil || repository != "e2e/remote" || len(actions) != 2 {
		t.Fatalf("scope = %q %v %v", repository, actions, err)
	}

	codec, _ := compress.Get("store")
	dataLayer, err := ociimg.NewLayer([]ociimg.LayerFile{{
		Path: "/blobs/000000", Mode: 0o644, Size: 4,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("data")), nil },
	}}, codec, 0)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := dataLayer.Digest()
	diffID, _ := dataLayer.DiffID()
	size, _ := dataLayer.Size()
	mediaType, _ := dataLayer.MediaType()
	r, _ := dataLayer.Compressed()
	compressed, _ := io.ReadAll(r)
	_ = r.Close()
	upload, err := sink.OpenBlob(context.Background(), reference, digest.String(), size)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Write(compressed); err != nil {
		t.Fatal(err)
	}
	if err := upload.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists, err := sink.BlobExists(context.Background(), reference, digest.String()); err != nil || !exists {
		t.Fatalf("exists = %v, %v", exists, err)
	}

	manifest := validRemoteManifest(t, digest.String(), size)
	var manifestJSON bytes.Buffer
	if err := index.WriteManifest(&manifestJSON, manifest); err != nil {
		t.Fatal(err)
	}
	var metadata bytes.Buffer
	if err := ociimg.BuildLayerTar(&metadata, []ociimg.LayerFile{{
		Path: "/backup/manifest.json", Mode: 0o644, Size: int64(manifestJSON.Len()),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(manifestJSON.Bytes())), nil },
	}}); err != nil {
		t.Fatal(err)
	}
	gotDigest, err := sink.CommitBackup(context.Background(), Backup{
		SessionID: "session",
		Start: &protocol.BackupStart{
			Reference: reference, ManifestJson: manifestJSON.Bytes(), MetadataLayer: metadata.Bytes(), LayerCount: 1,
			Platforms: []*protocol.Platform{{Os: "linux", Architecture: "amd64"}},
		},
		Layers: []Layer{{Index: 0, Size: size, Digest: digest.String(), DiffID: diffID.String(), MediaType: string(mediaType)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(ref, remote.WithTransport(srv.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != desc.Digest.String() || desc.MediaType != types.OCIImageIndex {
		t.Fatalf("published = %s %s, remote = %s %s", gotDigest, types.OCIImageIndex, desc.Digest, desc.MediaType)
	}
}

func TestRegistrySinkValidation(t *testing.T) {
	if _, err := NewRegistrySink(RegistrySinkOptions{}); err == nil {
		t.Fatal("nil broker accepted")
	}
	broker := NewTokenBroker(time.Second)
	sink, _ := NewRegistrySink(RegistrySinkOptions{Broker: broker})
	if _, _, err := sink.TokenScope("%%%"); err == nil {
		t.Fatal("invalid reference accepted")
	}
	if _, err := sink.CommitBackup(context.Background(), Backup{}); err == nil {
		t.Fatal("empty backup accepted")
	}
	for _, codec := range []string{"store", "zstd", "gzip", "xz", "lz4"} {
		if mt, err := layerMediaType(codec); err != nil || mt == "" {
			t.Fatalf("media type %s = %q, %v", codec, mt, err)
		}
	}
	if _, err := layerMediaType("missing"); err == nil {
		t.Fatal("unknown codec accepted")
	}
}

func validRemoteManifest(t *testing.T, digest string, size int64) *index.Manifest {
	t.Helper()
	return &index.Manifest{
		SchemaVersion: 1,
		Tool:          index.ToolInfo{Name: "backimage", Version: "test"},
		CreatedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Archive:       index.ArchiveInfo{Format: "tar", Compression: "store"},
		Chunking:      index.ChunkingInfo{Strategy: "length", TargetChunkBytes: 1024, Count: 1},
		Layers:        []index.LayerInfo{{Index: 0, Digest: digest, StoredBytes: size, ChunkFrom: 0, ChunkTo: 0}},
		Index:         index.Ref{Path: "index.json.zst"},
	}
}
