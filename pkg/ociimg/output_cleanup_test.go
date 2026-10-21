package ociimg

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// brokenLayer names a blob and fails while it is being copied, the way a full
// disk or a vanished spool file fails the write half-way.
type brokenLayer struct{}

var errBrokenLayer = errors.New("layer source vanished")

func (brokenLayer) Digest() (v1.Hash, error) {
	return v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("1", 64)}, nil
}
func (l brokenLayer) DiffID() (v1.Hash, error) { return l.Digest() }
func (brokenLayer) Size() (int64, error)       { return 4096, nil }
func (brokenLayer) MediaType() (types.MediaType, error) {
	return types.OCIUncompressedLayer, nil
}
func (brokenLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader(strings.NewReader("partial"), errReader{})), nil
}
func (l brokenLayer) Uncompressed() (io.ReadCloser, error) { return l.Compressed() }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errBrokenLayer }

func brokenImage(t *testing.T) (v1.Image, v1.ImageIndex) {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, brokenLayer{})
	if err != nil {
		t.Fatal(err)
	}
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
		Add: img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
	})
	return img, idx
}

// A failed layout write left oci-layout and the blobs already written, with no
// index.json: a directory that looks like a layout and is not one.
func TestAFailedLayoutWriteLeavesNothingItCreated(t *testing.T) {
	img, idx := brokenImage(t)
	ref, _ := name.ParseReference("example.test/team/p:t1")
	opts := WriterOptions{Images: map[string]v1.Image{"linux/amd64": img}, Runtime: v1.Platform{OS: "linux", Architecture: "amd64"}}
	write := func(t *testing.T, target Target, path string) {
		t.Helper()
		w, err := NewWriter(target, path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(context.Background(), ref, idx, nil); !errors.Is(err, errBrokenLayer) {
			t.Fatalf("write = %v, want the layer failure", err)
		}
	}

	t.Run("new layout", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "layout")
		write(t, TargetOCILayout, path)
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("the failed write left %s behind (%v)", path, err)
		}
	})
	t.Run("empty directory", func(t *testing.T) {
		path := t.TempDir()
		write(t, TargetOCILayout, path)
		if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
			t.Fatalf("the failed write left %d entries in the directory it was given (%v)", len(entries), err)
		}
	})
	t.Run("directory with content", func(t *testing.T) {
		path := t.TempDir()
		mine := filepath.Join(path, "mine.txt")
		if err := os.WriteFile(mine, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		write(t, TargetOCILayout, path)
		if data, err := os.ReadFile(mine); err != nil || string(data) != "keep" {
			t.Fatalf("a file that was already there was touched: %q, %v", data, err)
		}
	})
	t.Run("new tar", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out.tar")
		write(t, TargetTar, path)
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("the failed write left a partial %s (%v)", path, err)
		}
	})
}
