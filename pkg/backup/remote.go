package backup

import (
	"fmt"
	"io"

	"github.com/fpierri/backimage/pkg/protocol"
	backremote "github.com/fpierri/backimage/pkg/remote"
	"github.com/google/go-containerregistry/pkg/v1"
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
