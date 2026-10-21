package backup

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	v1remote "github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/registry"
	backremote "github.com/manprint/backimage/pkg/remote"
)

func (b *builder) remotePayload(images map[string]v1.Image) (backremote.Backup, error) {
	var out backremote.Backup
	platforms := b.platforms()
	if len(platforms) == 0 {
		return out, fmt.Errorf("no target platforms")
	}
	first := images[platString(platforms[0])]
	if first == nil {
		return out, fmt.Errorf("platform image %s is missing", platString(platforms[0]))
	}
	layers, err := first.Layers()
	if err != nil {
		return out, err
	}
	if len(layers) < 2 {
		return out, fmt.Errorf("platform image has %d layers, want executable and metadata", len(layers))
	}
	metaReader, err := layers[1].Compressed()
	if err != nil {
		return out, err
	}
	metadata, readErr := io.ReadAll(io.LimitReader(metaReader, protocol.MaxFrameSize+1))
	closeErr := metaReader.Close()
	if readErr != nil {
		return out, readErr
	}
	if closeErr != nil {
		return out, closeErr
	}
	if len(metadata) > protocol.MaxFrameSize {
		return out, fmt.Errorf("metadata layer is %d bytes, exceeds control frame limit %d", len(metadata), protocol.MaxFrameSize)
	}
	wirePlatforms := make([]*protocol.Platform, 0, len(platforms))
	for _, platform := range platforms {
		wirePlatforms = append(wirePlatforms, &protocol.Platform{Os: platform.OS, Architecture: platform.Architecture})
	}
	var estimated uint64
	for i, layer := range b.data {
		size, err := layer.Size()
		if err != nil {
			return out, fmt.Errorf("layer %d size: %w", i, err)
		}
		estimated += uint64(size)
	}
	out.Start = &protocol.BackupStart{
		Reference: b.cfg.Ref, ManifestJson: append([]byte(nil), b.manifestBytes...),
		LayerCount: uint32(len(b.data)), EstimatedBytes: estimated,
		Platforms: wirePlatforms, MetadataLayer: metadata,
	}
	out.Layers = append([]v1.Layer(nil), b.data...)
	return out, nil
}

// republishOwnSelfExtract answers a v1 receiver that published an index other
// than the one this client built. The client never sends its extractors (each
// is larger than a frame), so a receiver of another build puts its own in
// layer 0 and the index digest differs even though everything else is the
// client's. Layer 0 is what docker run executes on a restore, the program
// that reads the passphrase: accepting the receiver's would let an untrusted
// receiver — the one the layers mode exists for — plant it unnoticed. So the
// published index is read back with the client's credentials, every other
// layer must be the client's, and the client then republishes its own image:
// data and metadata are already there and only layer 0, configs and
// manifests travel. Any other difference stays an integrity failure and
// nothing is republished.
func (b *builder) republishOwnSelfExtract(ctx context.Context, idx v1.ImageIndex, images map[string]v1.Image, expected, published string) error {
	differs := fmt.Sprintf("remote index digest %s differs from local %s", published, expected)
	ref, err := name.ParseReference(b.cfg.Ref)
	if err != nil {
		return err
	}
	hash, err := v1.NewHash(published)
	if err != nil {
		return fmt.Errorf("%s: the receiver reported an invalid digest: %w", differs, registry.ErrPublishedMismatch)
	}
	kc := b.keychain()
	// By digest, not by tag: what is checked is exactly what the receiver
	// claimed, whatever the tag points to by now.
	remoteIdx, err := v1remote.Index(ref.Context().Digest(hash.String()),
		v1remote.WithContext(ctx), v1remote.WithAuthFromKeychain(kc))
	if err != nil {
		return fmt.Errorf("%s: reading what the receiver published: %w: %w", differs, registry.ErrPublishedMismatch, err)
	}
	if err := onlySelfExtractDiffers(idx, remoteIdx); err != nil {
		return fmt.Errorf("%s: %w: %w", differs, err, registry.ErrPublishedMismatch)
	}
	b.cfg.warn("backup: il ricevente ha pubblicato un /backimage (self-extract) diverso da quello di questo client, " +
		"con metadati e dati identici; capita con un listen-remote di un'altra versione, ma da qui non si distingue da un " +
		"estrattore sostituito: ripubblicazione dell'immagine con il self-extract di questo client")
	// The receiver's upload is what the counters describe; the repair re-checks
	// blobs the receiver already sent and must not count them again. It always
	// reads back the quick way, which is what proves the tag moved, and leaves
	// no checkpoint behind.
	skipped, skippedBytes, uploadedBytes := b.skipped, b.skippedBytes, b.uploadedBytes
	err = b.pushImages(ctx, ref, idx, images, b.basePushOptions())
	b.skipped, b.skippedBytes, b.uploadedBytes = skipped, skippedBytes, uploadedBytes
	if err != nil {
		return fmt.Errorf("%s, and republishing with this client's self-extract failed: the tag still holds the receiver's /backimage, "+
			"do not docker run that image; run the backup again: %w: %w", differs, registry.ErrPublishedMismatch, err)
	}
	b.report("backup: immagine ripubblicata con il self-extract di questo client; il tag risolve al digest locale")
	return nil
}

// onlySelfExtractDiffers accepts published only when it is local up to layer
// 0 of each platform and what derives from it (config, manifests, index):
// the same platforms, and every other layer the same blob.
func onlySelfExtractDiffers(local, published v1.ImageIndex) error {
	localManifest, err := local.IndexManifest()
	if err != nil {
		return err
	}
	publishedManifest, err := published.IndexManifest()
	if err != nil {
		return fmt.Errorf("published index: %w", err)
	}
	if len(publishedManifest.Manifests) != len(localManifest.Manifests) {
		return fmt.Errorf("the receiver published %d platforms, the client built %d",
			len(publishedManifest.Manifests), len(localManifest.Manifests))
	}
	byPlatform := make(map[string]v1.Descriptor, len(publishedManifest.Manifests))
	for _, desc := range publishedManifest.Manifests {
		if desc.Platform == nil {
			return errors.New("the receiver published a manifest without a platform")
		}
		key := desc.Platform.String()
		if _, dup := byPlatform[key]; dup {
			return fmt.Errorf("the receiver published platform %s twice", key)
		}
		byPlatform[key] = desc
	}
	for _, desc := range localManifest.Manifests {
		key := desc.Platform.String()
		pub, ok := byPlatform[key]
		if !ok {
			return fmt.Errorf("the receiver did not publish platform %s", key)
		}
		want, err := layerDescriptors(local, desc.Digest)
		if err != nil {
			return err
		}
		got, err := layerDescriptors(published, pub.Digest)
		if err != nil {
			return fmt.Errorf("published %s: %w", key, err)
		}
		if len(got) != len(want) {
			return fmt.Errorf("platform %s: the receiver published %d layers, the client built %d", key, len(got), len(want))
		}
		for i := 1; i < len(want); i++ {
			if got[i].Digest != want[i].Digest || got[i].Size != want[i].Size || got[i].MediaType != want[i].MediaType {
				return fmt.Errorf("platform %s: layer %d is %s, the client built %s", key, i, got[i].Digest, want[i].Digest)
			}
		}
	}
	return nil
}

func layerDescriptors(idx v1.ImageIndex, digest v1.Hash) ([]v1.Descriptor, error) {
	img, err := idx.Image(digest)
	if err != nil {
		return nil, err
	}
	manifest, err := img.Manifest()
	if err != nil {
		return nil, err
	}
	return manifest.Layers, nil
}
