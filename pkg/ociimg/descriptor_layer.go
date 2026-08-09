package ociimg

import (
	"errors"
	"io"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

var ErrLayerContentUnavailable = errors.New("layer content is already remote")

// DescriptorLayer is a metadata-only v1.Layer for a blob already present in
// the target registry. It lets the server build manifests without downloading
// or spooling the data layer.
type DescriptorLayer struct {
	digest    v1.Hash
	diffID    v1.Hash
	size      int64
	mediaType types.MediaType
}

func NewDescriptorLayer(digest, diffID string, size int64, mediaType types.MediaType) (*DescriptorLayer, error) {
	d, err := v1.NewHash(digest)
	if err != nil {
		return nil, err
	}
	diff, err := v1.NewHash(diffID)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, errors.New("negative layer size")
	}
	if mediaType == "" {
		return nil, errors.New("layer media type is required")
	}
	return &DescriptorLayer{digest: d, diffID: diff, size: size, mediaType: mediaType}, nil
}

func (l *DescriptorLayer) Digest() (v1.Hash, error)            { return l.digest, nil }
func (l *DescriptorLayer) DiffID() (v1.Hash, error)            { return l.diffID, nil }
func (l *DescriptorLayer) Size() (int64, error)                { return l.size, nil }
func (l *DescriptorLayer) MediaType() (types.MediaType, error) { return l.mediaType, nil }
func (l *DescriptorLayer) Compressed() (io.ReadCloser, error)  { return nil, ErrLayerContentUnavailable }
func (l *DescriptorLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, ErrLayerContentUnavailable
}
