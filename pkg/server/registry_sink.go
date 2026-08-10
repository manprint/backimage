package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/fpierri/backimage/pkg/compress"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/ociimg"
	"github.com/fpierri/backimage/pkg/protocol"
	backregistry "github.com/fpierri/backimage/pkg/registry"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type RegistrySinkOptions struct {
	Broker      *TokenBroker
	ChunkSize   int
	Jobs        int
	SelfExtract func(architecture string) ([]byte, error)
}

// RegistrySink streams layers directly to their destination repository.
type RegistrySink struct{ opts RegistrySinkOptions }

func NewRegistrySink(opts RegistrySinkOptions) (*RegistrySink, error) {
	if opts.Broker == nil {
		return nil, errors.New("registry token broker is required")
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 32 << 20
	}
	if opts.Jobs <= 0 {
		opts.Jobs = 3
	}
	return &RegistrySink{opts: opts}, nil
}

func (s *RegistrySink) ProvideToken(tok *protocol.Token) { s.opts.Broker.ProvideToken(tok) }

func (s *RegistrySink) TokenScope(reference string) (string, []string, error) {
	ref, err := name.ParseReference(reference, name.WeakValidation)
	if err != nil {
		return "", nil, err
	}
	return ref.Context().RepositoryStr(), []string{"pull", "push"}, nil
}

func (s *RegistrySink) KnownBlobs(context.Context, string) ([]string, error) {
	// A stateless server cannot map session_id to a repository before
	// BackupStart. Per-layer HEAD checks provide restart-safe resumption.
	return nil, nil
}

func (s *RegistrySink) BlobExists(ctx context.Context, reference, digest string) (bool, error) {
	client, err := s.client(ctx, reference)
	if err != nil {
		return false, err
	}
	return client.Exists(digest)
}

func (s *RegistrySink) OpenBlob(ctx context.Context, reference, digest string, _ int64) (BlobWriter, error) {
	client, err := s.client(ctx, reference)
	if err != nil {
		return nil, err
	}
	return client.Open(digest)
}

func (s *RegistrySink) client(ctx context.Context, reference string) (*backregistry.BlobClient, error) {
	ref, err := name.ParseReference(reference, name.WeakValidation)
	if err != nil {
		return nil, err
	}
	return backregistry.NewBlobClient(ctx, ref, s.opts.Broker, s.opts.ChunkSize)
}

func (s *RegistrySink) CommitBackup(ctx context.Context, backup Backup) (string, error) {
	if backup.Start == nil || len(backup.Start.MetadataLayer) == 0 {
		return "", errors.New("client metadata layer is required")
	}
	manifest, err := index.ReadManifest(bytes.NewReader(backup.Start.ManifestJson))
	if err != nil {
		return "", err
	}
	dataLayers := make([]v1.Layer, 0, len(backup.Layers))
	for i, layer := range backup.Layers {
		diffID := layer.DiffID
		if diffID == "" {
			return "", fmt.Errorf("layer %d has no diff_id", i)
		}
		mediaType := types.MediaType(layer.MediaType)
		if mediaType == "" {
			mediaType, err = layerMediaType(manifest.Archive.Compression)
			if err != nil {
				return "", err
			}
		}
		descriptor, err := ociimg.NewDescriptorLayer(layer.Digest, diffID, layer.Size, mediaType)
		if err != nil {
			return "", fmt.Errorf("layer %d descriptor: %w", i, err)
		}
		dataLayers = append(dataLayers, descriptor)
	}
	metadataLayer := static.NewLayer(append([]byte(nil), backup.Start.MetadataLayer...), types.OCIUncompressedLayer)
	platforms := backup.Start.Platforms
	if len(platforms) == 0 {
		platforms = []*protocol.Platform{{Os: "linux", Architecture: "amd64"}, {Os: "linux", Architecture: "arm64"}}
	}
	images := make(map[string]v1.Image, len(platforms))
	built := make([]ociimg.BuiltImage, 0, len(platforms))
	for _, p := range platforms {
		if p == nil || p.Os == "" || p.Architecture == "" {
			return "", errors.New("invalid target platform")
		}
		exe, err := s.selfExtract(backup.Start, p.Architecture)
		if err != nil {
			return "", err
		}
		platform := v1.Platform{OS: p.Os, Architecture: p.Architecture}
		img, err := ociimg.BuildImageFromExistingLayers(ociimg.ExistingLayerOptions{
			Platform: platform, SelfExtract: exe, MetadataLayer: metadataLayer,
			DataLayers: dataLayers, Manifest: manifest, Created: manifest.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
		if err != nil {
			return "", err
		}
		key := p.Os + "/" + p.Architecture
		images[key] = img
		built = append(built, ociimg.BuiltImage{Platform: platform, Image: img})
	}
	idx, err := ociimg.BuildIndex(built)
	if err != nil {
		return "", err
	}
	ref, err := name.ParseReference(backup.Start.Reference, name.WeakValidation)
	if err != nil {
		return "", err
	}
	if err := backregistry.PushWithProvider(ctx, ref, images, idx, s.opts.Broker, backregistry.PushOptions{
		Jobs: s.opts.Jobs, ChunkSize: int64(s.opts.ChunkSize), MaxRetries: 5,
	}); err != nil {
		return "", err
	}
	raw, err := idx.RawManifest()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CommitStream publishes a backup whose metadata was assembled here, from a
// raw stream the client never turned into layers.
func (s *RegistrySink) CommitStream(ctx context.Context, commit StreamCommit) (string, error) {
	if commit.Manifest == nil {
		return "", errors.New("server manifest is required")
	}
	dataLayers := make([]v1.Layer, 0, len(commit.Layers))
	for i, layer := range commit.Layers {
		descriptor, err := ociimg.NewDescriptorLayer(layer.Digest, layer.DiffID, layer.Size, types.MediaType(layer.MediaType))
		if err != nil {
			return "", fmt.Errorf("layer %d descriptor: %w", i, err)
		}
		dataLayers = append(dataLayers, descriptor)
	}
	platforms := commit.Start.GetPlatforms()
	if len(platforms) == 0 {
		platforms = []*protocol.Platform{{Os: "linux", Architecture: "amd64"}, {Os: "linux", Architecture: "arm64"}}
	}
	images := make(map[string]v1.Image, len(platforms))
	built := make([]ociimg.BuiltImage, 0, len(platforms))
	created := commit.Manifest.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	for _, p := range platforms {
		if p == nil || p.Os == "" || p.Architecture == "" {
			return "", errors.New("invalid target platform")
		}
		exe, err := s.streamSelfExtract(p.Architecture)
		if err != nil {
			return "", err
		}
		platform := v1.Platform{OS: p.Os, Architecture: p.Architecture}
		img, err := ociimg.BuildImage(ociimg.BuildOptions{
			Platform:    platform,
			SelfExtract: exe,
			Runnable:    commit.Start.GetRunnable(),
			Manifest:    commit.Manifest,
			ChunkTable:  commit.ChunkTable,
			IndexBlob:   commit.IndexBlob,
			KeyFiles:    commit.KeyFiles,
			DataLayers:  dataLayers,
			Codec:       commit.Codec,
			Created:     created,
		})
		if err != nil {
			return "", err
		}
		images[p.Os+"/"+p.Architecture] = img
		built = append(built, ociimg.BuiltImage{Platform: platform, Image: img})
	}
	idx, err := ociimg.BuildIndex(built)
	if err != nil {
		return "", err
	}
	ref, err := name.ParseReference(commit.Reference, name.WeakValidation)
	if err != nil {
		return "", err
	}
	if err := backregistry.PushWithProvider(ctx, ref, images, idx, s.opts.Broker, backregistry.PushOptions{
		Jobs: s.opts.Jobs, ChunkSize: int64(s.opts.ChunkSize), MaxRetries: 5,
	}); err != nil {
		return "", err
	}
	raw, err := idx.RawManifest()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// streamSelfExtract returns the bootstrap binary embedded in this server: a
// streaming client never uploads one.
func (s *RegistrySink) streamSelfExtract(arch string) ([]byte, error) {
	if s.opts.SelfExtract == nil {
		return nil, fmt.Errorf("this server has no self-extract binary for %s", arch)
	}
	data, err := s.opts.SelfExtract(arch)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("self-extract binary for %s is required", arch)
	}
	return data, nil
}

func (s *RegistrySink) selfExtract(start *protocol.BackupStart, arch string) ([]byte, error) {
	var data []byte
	switch arch {
	case "amd64":
		data = start.SelfextractAmd64
	case "arm64":
		data = start.SelfextractArm64
	default:
		return nil, fmt.Errorf("unsupported runnable platform linux/%s", arch)
	}
	if len(data) == 0 && s.opts.SelfExtract != nil {
		return s.opts.SelfExtract(arch)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("self-extract binary for %s is required", arch)
	}
	return append([]byte(nil), data...), nil
}

func layerMediaType(codecName string) (types.MediaType, error) {
	codec, err := compress.Get(strings.TrimSpace(codecName))
	if err != nil {
		return "", err
	}
	mediaType := types.OCIUncompressedLayer
	if suffix := codec.MediaTypeSuffix(); suffix != "" && suffix != "none" {
		mediaType = types.MediaType(string(types.OCIUncompressedLayer) + "+" + suffix)
	}
	return mediaType, nil
}
