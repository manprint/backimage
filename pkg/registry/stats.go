package registry

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// RepositoryStats describes the blobs reachable from every tag in one OCI
// repository. StorageBytes counts each digest once; ReferencedBytes counts it
// once per tag/platform reference and exposes the registry-side sharing.
type RepositoryStats struct {
	Tags            int   `json:"tags"`
	UniqueBlobs     int   `json:"uniqueBlobs"`
	StorageBytes    int64 `json:"storageBytes"`
	ReferencedBytes int64 `json:"referencedBytes"`
	SharedBlobs     int   `json:"sharedBlobs"`
	SharedBytes     int64 `json:"sharedBytes"`
}

type countedBlob struct {
	size  int64
	count int
}

// Stats reads repository manifests and counts shared config/layer blobs. It
// does not download layer payloads.
func Stats(ctx context.Context, repo name.Repository, kc Keychain) (RepositoryStats, error) {
	opts := []remote.Option{remote.WithContext(ctx)}
	if kc != nil {
		opts = append(opts, remote.WithAuthFromKeychain(kc))
	}
	tags, err := remote.List(repo, opts...)
	if err != nil {
		return RepositoryStats{}, fmt.Errorf("listing tags: %w", err)
	}
	sort.Strings(tags)
	counts := map[string]countedBlob{}
	for _, tag := range tags {
		ref, err := name.NewTag(repo.Name() + ":" + tag)
		if err != nil {
			return RepositoryStats{}, err
		}
		desc, err := remote.Get(ref, opts...)
		if err != nil {
			return RepositoryStats{}, fmt.Errorf("reading tag %s: %w", tag, err)
		}
		if idx, idxErr := desc.ImageIndex(); idxErr == nil {
			if err := countIndex(idx, counts); err != nil {
				return RepositoryStats{}, fmt.Errorf("counting tag %s: %w", tag, err)
			}
			continue
		}
		img, err := desc.Image()
		if err != nil {
			return RepositoryStats{}, fmt.Errorf("reading image %s: %w", tag, err)
		}
		if err := countImage(img, counts); err != nil {
			return RepositoryStats{}, fmt.Errorf("counting tag %s: %w", tag, err)
		}
	}
	stats := RepositoryStats{Tags: len(tags), UniqueBlobs: len(counts)}
	for _, blob := range counts {
		stats.StorageBytes += blob.size
		stats.ReferencedBytes += blob.size * int64(blob.count)
		if blob.count > 1 {
			stats.SharedBlobs++
			stats.SharedBytes += blob.size
		}
	}
	return stats, nil
}

func countIndex(idx v1.ImageIndex, counts map[string]countedBlob) error {
	manifest, err := idx.IndexManifest()
	if err != nil {
		return err
	}
	for _, desc := range manifest.Manifests {
		img, err := idx.Image(desc.Digest)
		if err == nil {
			if err := countImage(img, counts); err != nil {
				return err
			}
			continue
		}
		child, childErr := idx.ImageIndex(desc.Digest)
		if childErr != nil {
			return err
		}
		if err := countIndex(child, counts); err != nil {
			return err
		}
	}
	return nil
}

func countImage(img v1.Image, counts map[string]countedBlob) error {
	config, err := img.ConfigName()
	if err != nil {
		return err
	}
	rawConfig, err := img.RawConfigFile()
	if err != nil {
		return err
	}
	addBlob(counts, config.String(), int64(len(rawConfig)))
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	for _, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			return err
		}
		size, err := layer.Size()
		if err != nil {
			return err
		}
		addBlob(counts, digest.String(), size)
	}
	return nil
}

func addBlob(counts map[string]countedBlob, digest string, size int64) {
	blob := counts[digest]
	if blob.count == 0 {
		blob.size = size
	}
	blob.count++
	counts[digest] = blob
}
