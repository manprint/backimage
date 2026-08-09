package ociimg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/fpierri/backimage/pkg/compress"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// fileLayer is a v1.Layer backed by a pre-compressed tar file on disk.
// The compressed bytes are streamed from the file on every read, so the
// working set of an image does not depend on the backup size (prescription
// 05). The caller owns the file lifetime: it must exist as long as the image
// is pushed or written, then be removed.
type fileLayer struct {
	path      string
	size      int64
	digest    v1.Hash
	diffID    v1.Hash
	mediaType types.MediaType
	codecID   compress.ID
}

func (l *fileLayer) Digest() (v1.Hash, error)            { return l.digest, nil }
func (l *fileLayer) DiffID() (v1.Hash, error)            { return l.diffID, nil }
func (l *fileLayer) Size() (int64, error)                { return l.size, nil }
func (l *fileLayer) MediaType() (types.MediaType, error) { return l.mediaType, nil }
func (l *fileLayer) LayerType() (types.MediaType, error) { return l.mediaType, nil }
func (l *fileLayer) Name() (string, error)               { return "", nil }

func (l *fileLayer) Compressed() (io.ReadCloser, error) {
	return os.Open(l.path)
}

func (l *fileLayer) Uncompressed() (io.ReadCloser, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	codec, err := compress.ByID(l.codecID)
	if err != nil {
		f.Close()
		return nil, err
	}
	dr, err := codec.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &closerR{Reader: dr, close: func() error {
		_ = dr.Close()
		return f.Close()
	}}, nil
}

type closerR struct {
	io.Reader
	close func() error
}

func (c *closerR) Close() error { return c.close() }

// NewFileLayer streams the tar of files through the codec straight into a
// file in dir. Memory use is bounded by the codec window, not by the layer
// size. It returns the layer; remove the file with Layer.Remove() when done.
func NewFileLayer(files []LayerFile, codec compress.Codec, level int, dir string) (v1.Layer, error) {
	if codec == nil {
		return nil, fmt.Errorf("nil codec")
	}
	min, _, def := codec.Levels()
	if level < 0 {
		level = def
	}
	_ = min

	tmp, err := os.CreateTemp(dir, "backimage-layer-*.blob")
	if err != nil {
		return nil, fmt.Errorf("layer temp: %w", err)
	}
	path := tmp.Name()
	fail := func(op string, err error) (v1.Layer, error) {
		tmp.Close()
		os.Remove(path)
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	rawH := sha256.New()
	cw, err := codec.NewWriter(tmp, level)
	if err != nil {
		return fail("codec writer", err)
	}
	if err := BuildLayerTar(io.MultiWriter(cw, rawH), files); err != nil {
		cw.Close()
		return fail("layer tar", err)
	}
	if err := cw.Close(); err != nil {
		return fail("codec close", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("temp close", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		return fail("stat", err)
	}
	// digest over the stored (compressed) stream
	h := sha256.New()
	cf, err := os.Open(path)
	if err != nil {
		return fail("reopen", err)
	}
	if _, err := io.Copy(h, cf); err != nil {
		cf.Close()
		return fail("digest", err)
	}
	cf.Close()

	suffix := codec.MediaTypeSuffix()
	mt := types.OCIUncompressedLayer
	if suffix != "" && suffix != "none" {
		mt = types.MediaType(string(types.OCIUncompressedLayer) + "+" + suffix)
	}
	dh := v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(h.Sum(nil))}
	rh := v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(rawH.Sum(nil))}
	return &fileLayer{
		path:      path,
		size:      st.Size(),
		digest:    dh,
		diffID:    rh,
		mediaType: mt,
		codecID:   codec.ID(),
	}, nil
}

// Remove deletes the layer's backing file. It is safe to call after the
// image has been pushed or written.
func RemoveLayer(l v1.Layer) {
	if fl, ok := l.(*fileLayer); ok {
		_ = os.Remove(fl.path)
	}
}
