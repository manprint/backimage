package backup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/fpierri/backimage/pkg/archive"
	"github.com/fpierri/backimage/pkg/chunk"
	"github.com/fpierri/backimage/pkg/crypt"
	"github.com/fpierri/backimage/pkg/protocol"
	backremote "github.com/fpierri/backimage/pkg/remote"
	"github.com/google/go-containerregistry/pkg/v1"
)

// RemoteStreamUploader is implemented by remote.Client. In streaming mode the
// local process only walks the filesystem and writes a tar stream: chunking,
// compression, encryption, layer assembly and the registry push all happen on
// the remote server, so local disk usage is independent of the backup size.
type RemoteStreamUploader interface {
	UploadStream(context.Context, backremote.StreamBackup) (backremote.Result, error)
}

// runStream performs a protocol v2 backup. Nothing is spooled locally.
func (b *builder) runStream(ctx context.Context, est Estimate, res Result) (Result, error) {
	cfg := b.cfg
	keyFiles, err := b.buildKeyFiles()
	if err != nil {
		return res, err
	}
	start := &protocol.StreamStart{
		Reference:      cfg.Ref,
		ToolVersion:    cfg.Version,
		Created:        b.createdAt.UTC().Format(time.RFC3339),
		Archive:        &protocol.ArchiveConfig{Compression: b.codec.Name(), Level: int32(b.level)},
		Encryption:     encryptionConfig(cfg, b.km, keyFiles),
		Platforms:      wirePlatforms(b.platforms()),
		Runnable:       cfg.Runnable,
		EstimatedBytes: uint64(est.Bytes),
		EstimatedFiles: est.Files,
		Sources:        streamSources(cfg),
		Host:           streamHost(cfg),
		Dedup:          cfg.Dedup,
		Cdc:            cdcToWire(b.cdcParams),
		MaxLayerBytes:  uint64(b.plan.LayerBytes),
	}
	var stats archive.Stats
	source := func(sourceCtx context.Context, w io.Writer) error {
		// The archiver writes in small bursts; buffering them into full frames
		// keeps the wire overhead and the client allocation rate flat.
		buffered := bufio.NewWriterSize(w, backremote.StreamFrameSize)
		writer := archive.NewWriter(buffered, archive.Options{
			Strict:         !cfg.AllowDegraded,
			OneFileSystem:  cfg.OneFileSystem,
			Excludes:       cfg.Exclude,
			NumericOwner:   cfg.NumericOwner,
			PreserveACLs:   true,
			PreserveXattrs: true,
		})
		for _, root := range cfg.RootPaths {
			if err := writer.AddRoot(sourceCtx, root); err != nil {
				return err
			}
		}
		st, err := writer.Close()
		if err != nil {
			return err
		}
		if err := buffered.Flush(); err != nil {
			return err
		}
		stats = st
		return nil
	}
	if cfg.Progress != nil {
		cfg.Progress("backup: streaming remoto in corso (pipeline sul server)")
	}
	remoteResult, err := cfg.RemoteStream.UploadStream(ctx, backremote.StreamBackup{
		Start: start, Source: source, Progress: streamProgress(cfg),
	})
	if err != nil {
		return res, fmt.Errorf("remote stream: %w", err)
	}
	if remoteResult.Digest == "" {
		return res, fmt.Errorf("remote server returned an empty digest")
	}
	res.Digest = remoteResult.Digest
	res.Files = stats.Files
	res.BytesRaw = stats.BytesRaw
	if remoteResult.Files > 0 {
		res.Files = remoteResult.Files
	}
	if remoteResult.RawBytes > 0 {
		res.BytesRaw = int64(remoteResult.RawBytes)
	}
	res.BytesStored = int64(remoteResult.StoredBytes)
	res.Layers = int(remoteResult.Layers)
	res.Chunks = int(remoteResult.Chunks)
	res.SkippedBlobs = int(remoteResult.BlobsSkipped)
	res.UploadedBytes = int64(remoteResult.BytesUploaded)
	if cfg.Progress != nil {
		cfg.Progress("backup: streaming remoto completato")
	}
	return res, nil
}

// streamProgress renders the server-side stages: reception, chunking,
// compression, encryption and push all happen there.
func streamProgress(cfg Config) func(*protocol.StreamProgress) {
	if cfg.Progress == nil {
		return nil
	}
	return func(p *protocol.StreamProgress) {
		cfg.Progress(fmt.Sprintf(
			"server[%s]: ricevuti %.1f MiB, archiviati %.1f MiB, caricati %.1f MiB, layer %d (%d saltati), chunk %d",
			p.GetStage(),
			float64(p.GetReceivedBytes())/(1<<20),
			float64(p.GetStoredBytes())/(1<<20),
			float64(p.GetUploadedBytes())/(1<<20),
			p.GetLayers(), p.GetLayersSkipped(), p.GetChunks(),
		))
	}
}

func encryptionConfig(cfg Config, km *crypt.KeyMaterial, keyFiles map[string][]byte) *protocol.EncryptionConfig {
	if !cfg.Encrypt || km == nil {
		return &protocol.EncryptionConfig{}
	}
	return &protocol.EncryptionConfig{
		Enabled:        true,
		Dek:            append([]byte(nil), km.DEK...),
		NonceKey:       append([]byte(nil), km.NonceKey...),
		NonceMode:      nonceMode(cfg.Dedup),
		KeyFiles:       keyFiles,
		Recipients:     cfg.Recipients,
		KeyFingerprint: keyFingerprint(km),
	}
}

func streamSources(cfg Config) []string {
	if cfg.NoMetadata {
		return nil
	}
	return append([]string(nil), cfg.RootPaths...)
}

func streamHost(cfg Config) *protocol.HostInfo {
	if cfg.NoMetadata {
		return &protocol.HostInfo{}
	}
	return &protocol.HostInfo{Hostname: hostname(), Os: runtime.GOOS, Arch: runtime.GOARCH}
}

func cdcToWire(p chunk.CDCParams) *protocol.CDCParams {
	return &protocol.CDCParams{
		Min: uint64(p.Min), Avg: uint64(p.Avg), Max: uint64(p.Max), Polynomial: p.Polynomial,
	}
}

func wirePlatforms(platforms []v1.Platform) []*protocol.Platform {
	out := make([]*protocol.Platform, 0, len(platforms))
	for _, platform := range platforms {
		out = append(out, &protocol.Platform{Os: platform.OS, Architecture: platform.Architecture})
	}
	return out
}

// wrapKeyFiles produces the age-wrapped key files stored inside the image. In
// streaming mode they are built locally: the passphrase never leaves the host.
func wrapKeyFiles(km *crypt.KeyMaterial, passphrase []byte, recipients []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if len(recipients) == 0 && passphrase == nil {
		return files, nil
	}
	write := func(name string, rcpt crypt.Recipients) error {
		var buf bytes.Buffer
		if err := crypt.WrapKeys(&buf, km, rcpt); err != nil {
			return err
		}
		files[name] = buf.Bytes()
		return nil
	}
	if passphrase != nil {
		if err := write("keys.pass.age", crypt.Recipients{Passphrase: passphrase}); err != nil {
			return nil, err
		}
	}
	if len(recipients) > 0 {
		if err := write("keys.age", crypt.Recipients{AgeKeys: recipients}); err != nil {
			return nil, err
		}
	}
	return files, nil
}
