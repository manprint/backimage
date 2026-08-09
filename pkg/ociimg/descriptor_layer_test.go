package ociimg

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/fpierri/backimage/pkg/index"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestDescriptorLayerAndExistingImage(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	diffID := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	data, err := NewDescriptorLayer(digest, diffID, 123, types.OCILayerZStd)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := data.Digest(); got.String() != digest {
		t.Fatalf("digest = %s", got)
	}
	if got, _ := data.DiffID(); got.String() != diffID {
		t.Fatalf("diffID = %s", got)
	}
	if got, _ := data.Size(); got != 123 {
		t.Fatalf("size = %d", got)
	}
	if _, err := data.Compressed(); !errors.Is(err, ErrLayerContentUnavailable) {
		t.Fatalf("compressed error = %v", err)
	}
	if _, err := data.Uncompressed(); !errors.Is(err, ErrLayerContentUnavailable) {
		t.Fatalf("uncompressed error = %v", err)
	}

	meta, err := NewLayer([]LayerFile{{Path: "/backup/manifest.json", Mode: 0o644, Size: 2, Open: bytesOpen([]byte("{}"))}}, mustStoreCodec(), 0)
	if err != nil {
		t.Fatal(err)
	}
	img, err := BuildImageFromExistingLayers(ExistingLayerOptions{
		Platform:    v1.Platform{OS: "linux", Architecture: "amd64"},
		SelfExtract: []byte("binary"), MetadataLayer: meta, DataLayers: []v1.Layer{data},
		Manifest: &index.Manifest{SchemaVersion: 1, Archive: index.ArchiveInfo{Compression: "zstd"}},
		Created:  "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 3 {
		t.Fatalf("layers = %d", len(layers))
	}
	gotData, _ := layers[2].Digest()
	if gotData.String() != digest {
		t.Fatalf("data digest = %s", gotData)
	}
}

func bytesOpen(data []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
}

func TestDescriptorLayerValidation(t *testing.T) {
	valid := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		digest, diff string
		size         int64
		media        types.MediaType
	}{
		{"bad", valid, 1, types.OCILayer},
		{valid, "bad", 1, types.OCILayer},
		{valid, valid, -1, types.OCILayer},
		{valid, valid, 1, ""},
	} {
		if _, err := NewDescriptorLayer(tc.digest, tc.diff, tc.size, tc.media); err == nil {
			t.Fatalf("invalid descriptor accepted: %#v", tc)
		}
	}
	if _, err := BuildImageFromExistingLayers(ExistingLayerOptions{}); err == nil {
		t.Fatal("empty image options accepted")
	}
	if _, err := BuildImageFromExistingLayers(ExistingLayerOptions{SelfExtract: []byte("x")}); err == nil {
		t.Fatal("missing metadata accepted")
	}
}
