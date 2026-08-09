package ociimg

import (
	"errors"
	"time"

	"github.com/fpierri/backimage/pkg/index"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ExistingLayerOptions builds an image around data blobs that the remote
// server already uploaded. MetadataLayer is the exact client-built layer.
type ExistingLayerOptions struct {
	Platform      v1.Platform
	SelfExtract   []byte
	MetadataLayer v1.Layer
	DataLayers    []v1.Layer
	Manifest      *index.Manifest
	Created       string
}

// BuildImageFromExistingLayers creates config and manifest blobs without
// opening any data layer content.
func BuildImageFromExistingLayers(opts ExistingLayerOptions) (v1.Image, error) {
	if len(opts.SelfExtract) == 0 {
		return nil, errNoEntrypoint
	}
	if opts.MetadataLayer == nil {
		return nil, errors.New("metadata layer is required")
	}
	exeLayer, err := buildExecutableLayer(opts.SelfExtract)
	if err != nil {
		return nil, err
	}
	all := make([]v1.Layer, 0, len(opts.DataLayers)+2)
	all = append(all, exeLayer, opts.MetadataLayer)
	all = append(all, opts.DataLayers...)
	for _, layer := range all {
		if layer == nil {
			return nil, errors.New("nil existing layer")
		}
	}
	img, err := mutate.AppendLayers(empty.Image, all...)
	if err != nil {
		return nil, err
	}
	created := opts.Created
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}
	codecName := "store"
	if opts.Manifest != nil && opts.Manifest.Archive.Compression != "" {
		codecName = opts.Manifest.Archive.Compression
	}
	labels := imageLabels(opts.Manifest, created, codecName)
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg.Architecture = opts.Platform.Architecture
	cfg.OS = opts.Platform.OS
	cfg.Config = v1.Config{
		Entrypoint: []string{executablePath}, Cmd: []string{"info"},
		WorkingDir: "/", User: "0:0", Labels: labels,
	}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		return nil, err
	}
	img = mutate.Annotations(img, labels).(v1.Image)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	return img, nil
}
