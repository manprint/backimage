package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/manprint/backimage/pkg/index"
)

// rawLayer serves a stream the test built by hand where a layer is expected.
type rawLayer struct {
	v1.Layer
	open func() io.Reader
}

func (l *rawLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(l.open()), nil
}

// zeros is an endless source. It stands in for a decompression bomb: a
// metadata layer is compressed, so the bytes a reader is asked to hold are
// produced by the decoder, not paid for by whoever uploaded the image.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// hostileMetaTar is a metadata layer holding one entry of the declared size,
// delivered by an endless generator so the test costs nothing to build and
// the bytes are really there for a reader willing to take them.
func hostileMetaTar(t *testing.T, size int64) func() io.Reader {
	t.Helper()
	var head bytes.Buffer
	tw := tar.NewWriter(&head)
	if err := tw.WriteHeader(&tar.Header{
		Name: "backup/index.json.zst", Typeflag: tar.TypeReg, Mode: 0o644, Size: size,
	}); err != nil {
		t.Fatal(err)
	}
	// Closing would demand the declared bytes up front; the header is what
	// the tar reader consumes before handing over the body.
	_ = tw.Flush()
	header := append([]byte(nil), head.Bytes()...)
	return func() io.Reader {
		return io.MultiReader(bytes.NewReader(header), io.LimitReader(zeros{}, size))
	}
}

// TestAMetadataLayerCannotAskForMoreThanAReaderWillHold is A7.2 on the path
// where nothing can measure the blob first: the metadata arrives inside a
// compressed layer, so its size is whatever the decoder produces.
func TestAMetadataLayerCannotAskForMoreThanAReaderWillHold(t *testing.T) {
	base, _, _, _ := sourceFixture(t)
	layers, err := base.Layers()
	if err != nil {
		t.Fatal(err)
	}
	hostile := &rawLayer{Layer: layers[1], open: hostileMetaTar(t, index.DefaultMaxMetadataBytes+1)}
	s, err := newImageSource(&layersImage{Image: base, layers: []v1.Layer{layers[0], hostile}}, SourceOptions{
		CacheDir: t.TempDir(), CacheSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = s.Manifest(context.Background())
	runtime.ReadMemStats(&after)

	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("refusing the declaration allocated %d bytes", allocated)
	}
	if !errors.Is(err, index.ErrBadSchema) {
		t.Fatalf("Manifest = %v, want ErrBadSchema", err)
	}
	if !strings.Contains(err.Error(), "index.json.zst") {
		t.Fatalf("the refusal must name the entry: %v", err)
	}
}

// TestTheHonestMetadataLayerIsUntouched is the control: the same reader, the
// same fixture, nothing crafted.
func TestTheHonestMetadataLayerIsUntouched(t *testing.T) {
	img, _, _, _ := sourceFixture(t)
	s, err := newImageSource(img, SourceOptions{CacheDir: t.TempDir(), CacheSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if m, err := s.Manifest(context.Background()); err != nil || m.Chunking.Count != 2 {
		t.Fatalf("manifest = %#v, %v", m, err)
	}
}
