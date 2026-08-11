package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/manprint/backimage/pkg/chunk"
	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/ociimg"
	"github.com/manprint/backimage/pkg/protocol"
)

// maxDataLayers is the OCI layer budget reserved for data layers: the image
// also carries the executable and the metadata layer.
const maxDataLayers = 118

// StreamCommit is everything the server assembled from one raw archive
// stream. The client never sees these artifacts before they are published.
type StreamCommit struct {
	SessionID  string
	Reference  string
	Start      *protocol.StreamStart
	Manifest   *index.Manifest
	ChunkTable *index.ChunkTable
	IndexBlob  []byte
	// PrivateBlob is the sealed confidential metadata of an encrypted backup.
	// It is empty when the session runs without encryption.
	PrivateBlob []byte
	KeyFiles    map[string][]byte
	Layers      []Layer
	Codec       compress.Codec
	Level       int
}

// StreamCommitter publishes the image built entirely on the server. A Sink
// that does not implement it cannot serve protocol v2 sessions.
type StreamCommitter interface {
	CommitStream(context.Context, StreamCommit) (string, error)
}

// StreamResult reports one completed streaming ingest.
type StreamResult struct {
	Digest        string
	RawBytes      uint64
	StoredBytes   uint64
	UploadedBytes uint64
	Layers        uint32
	LayersSkipped uint32
	Chunks        uint32
	Files         int64
}

// stream stages reported to the client. They map one to one onto the pipeline
// steps the operator expects to observe on the server.
const (
	stageReceiving  = "receiving"
	stagePushing    = "pushing"
	stagePublishing = "publishing"
	stageDone       = "done"
)

type ingestConfig struct {
	Start     *protocol.StreamStart
	SessionID string
	Reference string
	TempDir   string
	MaxBytes  uint64
	Sink      Sink
	Metrics   *Metrics
	Now       func() time.Time
}

// streamStats is shared with the session loop, which reports progress to the
// client while the pipeline keeps running.
type streamStats struct {
	received atomic.Uint64
	stored   atomic.Uint64
	uploaded atomic.Uint64
	layers   atomic.Uint32
	skipped  atomic.Uint32
	chunks   atomic.Uint32
	stage    atomic.Value
}

func (s *streamStats) setStage(stage string) { s.stage.Store(stage) }

func (s *streamStats) snapshot() *protocol.StreamProgress {
	stage, _ := s.stage.Load().(string)
	return &protocol.StreamProgress{
		Stage:         stage,
		ReceivedBytes: s.received.Load(),
		StoredBytes:   s.stored.Load(),
		UploadedBytes: s.uploaded.Load(),
		Layers:        s.layers.Load(),
		LayersSkipped: s.skipped.Load(),
		Chunks:        s.chunks.Load(),
	}
}

// errStreamAborted stops the pipeline when the client disappears.
var errStreamAborted = errors.New("stream aborted")

// ingest owns the server-side pipeline of one session: it consumes the raw
// archive frames and performs chunking, compression, encryption, layer
// assembly and the registry upload without ever asking the client for more
// than the plain byte stream.
type ingest struct {
	cfg   ingestConfig
	stats streamStats
	pw    *io.PipeWriter
	done  chan struct{}

	mu  sync.Mutex
	res StreamResult
	err error
}

func startIngest(ctx context.Context, cfg ingestConfig) (*ingest, error) {
	if cfg.Sink == nil {
		return nil, errors.New("server sink is required")
	}
	if _, ok := cfg.Sink.(StreamCommitter); !ok {
		return nil, errors.New("this server cannot publish streamed backups")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	in := &ingest{cfg: cfg, done: make(chan struct{})}
	// The policy is validated before the session acknowledges the stream: a
	// bad codec or key must never cost the client a single uploaded byte.
	builder, err := newStreamBuilder(cfg, &in.stats)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	in.pw = pw
	in.stats.setStage(stageReceiving)
	go func() {
		defer close(in.done)
		defer builder.cleanup()
		res, err := builder.run(ctx, pr)
		// Unblock (and fail) any pending client write with the real cause.
		if err != nil {
			_ = pr.CloseWithError(err)
		} else {
			_ = pr.Close()
		}
		in.mu.Lock()
		in.res, in.err = res, err
		in.mu.Unlock()
	}()
	return in, nil
}

// Write feeds one received data frame into the pipeline. It blocks while the
// pipeline is busy, which is the back-pressure the transport needs.
func (i *ingest) Write(p []byte) error {
	n, err := i.pw.Write(p)
	i.stats.received.Add(uint64(n))
	if err != nil {
		return i.cause(err)
	}
	return nil
}

// Finish closes the stream and waits for the publish step.
func (i *ingest) Finish() (StreamResult, error) {
	_ = i.pw.Close()
	<-i.done
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.res, i.err
}

// Abort tears the pipeline down and releases every temporary resource.
func (i *ingest) Abort() {
	_ = i.pw.CloseWithError(errStreamAborted)
	<-i.done
}

func (i *ingest) progress() *protocol.StreamProgress { return i.stats.snapshot() }

func (i *ingest) cause(err error) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.err != nil {
		return i.err
	}
	return err
}

// streamBuilder is the server-side twin of the local backup builder.
type streamBuilder struct {
	cfg    ingestConfig
	stats  *streamStats
	codec  compress.Codec
	level  int
	plan   chunk.Plan
	cdc    chunk.CDCParams
	km     *crypt.KeyMaterial
	sealer crypt.Sealer

	boundary         chunk.LayerBoundary
	boundaryFallback bool

	spool     *spoolFile
	rows      []index.Chunk
	layerInfo []index.LayerInfo
	layers    []Layer
	chunkIdx  int
	plain     int64
	stored    int64
	created   time.Time
}

func newStreamBuilder(cfg ingestConfig, stats *streamStats) (*streamBuilder, error) {
	start := cfg.Start
	codec, err := compress.Get(compressionName(start))
	if err != nil {
		return nil, err
	}
	level := int(start.GetArchive().GetLevel())
	if level == 0 {
		_, _, def := codec.Levels()
		level = def
	}
	limits := chunk.DefaultLimits()
	if start.GetMaxLayerBytes() > 0 {
		limits.TargetLayerBytes = int64(start.GetMaxLayerBytes())
	}
	plan, err := chunk.PlanLayers(int64(start.GetEstimatedBytes()), limits)
	if err != nil {
		return nil, err
	}
	cdc, err := chunk.NormalizeCDCParams(cdcFromWire(start.GetCdc()))
	if err != nil {
		return nil, err
	}
	if start.GetDedup() {
		plan.ChunkBytes = cdc.Avg
	}
	created := time.Now().UTC()
	if value := strings.TrimSpace(start.GetCreated()); value != "" {
		created, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return nil, fmt.Errorf("created timestamp: %w", err)
		}
	}
	b := &streamBuilder{
		cfg: cfg, stats: stats, codec: codec, level: level,
		plan: plan, cdc: cdc, created: created.UTC(),
	}
	if enc := start.GetEncryption(); enc.GetEnabled() {
		km, kmErr := keyMaterialFromWire(enc)
		if kmErr != nil {
			return nil, kmErr
		}
		mode := crypt.NonceRandom
		if enc.GetNonceMode() == "convergent" {
			mode = crypt.NonceConvergent
		}
		sealer, sealErr := crypt.NewSealer(km, mode)
		if sealErr != nil {
			km.Wipe()
			return nil, sealErr
		}
		b.km, b.sealer = km, sealer
	}
	return b, nil
}

func (b *streamBuilder) run(ctx context.Context, r io.Reader) (StreamResult, error) {
	var res StreamResult
	scanned := make(chan scanOutcome, 1)
	scanReader, scanWriter := io.Pipe()
	go func() {
		entries, stats, err := scanArchive(scanReader)
		// Keep draining so the chunker is never blocked by a failed scan. A
		// drain error is irrelevant: the pipeline reports the real cause.
		if _, drainErr := io.Copy(io.Discard, scanReader); drainErr != nil {
			_ = drainErr
		}
		_ = scanReader.Close()
		scanned <- scanOutcome{entries: entries, stats: stats, err: err}
	}()

	source := io.TeeReader(r, scanWriter)
	splitter, err := b.splitter(source)
	if err != nil {
		_ = scanWriter.CloseWithError(err)
		<-scanned
		return res, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = scanWriter.CloseWithError(err)
			<-scanned
			return res, err
		}
		ck, err := splitter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = scanWriter.CloseWithError(err)
			<-scanned
			return res, fmt.Errorf("split: %w", err)
		}
		stored, err := b.storeChunk(ck)
		if err != nil {
			_ = scanWriter.CloseWithError(err)
			<-scanned
			return res, err
		}
		if b.cfg.MaxBytes > 0 && uint64(b.plain) > b.cfg.MaxBytes {
			err := fmt.Errorf("session quota exceeded: %d > %d", b.plain, b.cfg.MaxBytes)
			_ = scanWriter.CloseWithError(err)
			<-scanned
			return res, err
		}
		if b.shouldRoll(sha256.Sum256(stored)) {
			if err := b.roll(ctx); err != nil {
				_ = scanWriter.CloseWithError(err)
				<-scanned
				return res, err
			}
		}
	}
	if err := b.roll(ctx); err != nil {
		_ = scanWriter.CloseWithError(err)
		<-scanned
		return res, err
	}
	_ = scanWriter.Close()
	outcome := <-scanned
	if outcome.err != nil {
		return res, outcome.err
	}
	if b.chunkIdx == 0 {
		return res, errors.New("the client stream produced no data")
	}

	b.stats.setStage(stagePublishing)
	digest, err := b.publish(ctx, outcome)
	if err != nil {
		return res, err
	}
	b.stats.setStage(stageDone)
	res = StreamResult{
		Digest:        digest,
		RawBytes:      uint64(outcome.stats.BytesRaw),
		StoredBytes:   uint64(b.stored),
		UploadedBytes: b.stats.uploaded.Load(),
		Layers:        uint32(len(b.layers)),
		LayersSkipped: b.stats.skipped.Load(),
		Chunks:        uint32(b.chunkIdx),
		Files:         outcome.stats.Files,
	}
	return res, nil
}

type scanOutcome struct {
	entries []index.FileEntry
	stats   scanStats
	err     error
}

func (b *streamBuilder) splitter(r io.Reader) (chunk.Splitter, error) {
	if b.cfg.Start.GetDedup() {
		b.boundary = chunk.NewContentBoundary(b.plan.LayerBytes, b.plan.LayerBytes/4, safeLayerMaximum(b.plan.LayerBytes))
		return chunk.NewCDC(r, b.cdc), nil
	}
	b.boundary = chunk.NewFixedBoundary(b.plan.LayerBytes)
	return chunk.NewFixed(r, b.plan.ChunkBytes)
}

func safeLayerMaximum(target int64) int64 {
	if target <= 0 || target > int64(^uint64(0)>>1)/4 {
		return int64(^uint64(0) >> 1)
	}
	return target * 4
}

// storeChunk compresses and seals one chunk into the per-layer spool. Only the
// current chunk and the current layer live on the server.
func (b *streamBuilder) storeChunk(ck *chunk.Chunk) ([]byte, error) {
	var buf bytes.Buffer
	w, err := b.codec.NewWriter(&buf, b.level)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(ck.Data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	stored := buf.Bytes()
	if b.sealer != nil {
		stored, err = b.sealer.Seal(nil, uint32(ck.Index), b.codec, buf.Bytes(), ck.PlainSHA)
		if err != nil {
			return nil, fmt.Errorf("seal chunk %d: %w", ck.Index, err)
		}
	}
	if b.spool == nil {
		spool, err := newSpool(b.cfg.TempDir, len(b.layers))
		if err != nil {
			return nil, err
		}
		b.spool = spool
	}
	if err := b.spool.Write(stored); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(stored)
	b.rows = append(b.rows, index.Chunk{
		I:  ck.Index,
		P:  "",
		Ps: "sha256:" + hex.EncodeToString(ck.PlainSHA[:]),
		Sb: int64(len(stored)),
		Pb: ck.PlainBytes,
		Ss: "sha256:" + hex.EncodeToString(sum[:]),
	})
	b.chunkIdx++
	b.plain += ck.PlainBytes
	b.stored += int64(len(stored))
	b.stats.chunks.Store(uint32(b.chunkIdx))
	b.stats.stored.Store(uint64(b.stored))
	return stored, nil
}

func (b *streamBuilder) shouldRoll(digest [32]byte) bool {
	if b.spool == nil || b.boundary == nil {
		return false
	}
	// Keep one unconstrained final layer: this is the hard guard behind the
	// probabilistic content-defined boundaries.
	if len(b.layers) >= maxDataLayers-1 {
		return false
	}
	return b.boundary.ShouldClose(b.spool.size, digest)
}

// roll turns the current spool into an OCI layer and streams it to the
// registry. Both temporary files are removed before the next layer starts.
func (b *streamBuilder) roll(ctx context.Context) error {
	if b.spool == nil {
		return nil
	}
	if len(b.layers) >= maxDataLayers {
		return fmt.Errorf("layer limit exceeded: %d", maxDataLayers)
	}
	blobDigest, err := b.spool.digest()
	if err != nil {
		b.spool.Remove()
		b.spool = nil
		return err
	}
	if err := b.spool.Close(); err != nil {
		b.spool.Remove()
		b.spool = nil
		return err
	}
	from := 0
	if len(b.layerInfo) > 0 {
		from = b.layerInfo[len(b.layerInfo)-1].ChunkTo + 1
	}
	to := b.chunkIdx - 1
	// Content-addressed name: an identical layer keeps an identical digest
	// even when an earlier probabilistic boundary moves.
	dataPath := dataBlobPath(blobDigest)
	for i := from; i <= to && i < len(b.rows); i++ {
		b.rows[i].P = dataPath
	}

	spool := b.spool
	b.spool = nil
	defer spool.Remove()
	layer, err := ociimg.NewFileLayer([]ociimg.LayerFile{{
		Path: "/" + dataPath,
		Mode: 0o644,
		Size: spool.size,
		Open: func() (io.ReadCloser, error) { return os.Open(spool.path) },
	}}, b.codec, b.level, b.cfg.TempDir)
	if err != nil {
		return err
	}
	defer ociimg.RemoveLayer(layer)

	descriptor, err := layerDescriptor(layer, uint32(len(b.layers)))
	if err != nil {
		return err
	}
	if err := b.upload(ctx, layer, descriptor); err != nil {
		return err
	}
	b.layers = append(b.layers, descriptor)
	b.layerInfo = append(b.layerInfo, index.LayerInfo{
		Index:       len(b.layerInfo),
		Digest:      blobDigest,
		ChunkFrom:   from,
		ChunkTo:     to,
		StoredBytes: spool.size,
	})
	b.stats.layers.Store(uint32(len(b.layers)))
	b.applyBoundaryFallback()
	return nil
}

func (b *streamBuilder) upload(ctx context.Context, layer ociimgLayer, descriptor Layer) error {
	exists, err := b.cfg.Sink.BlobExists(ctx, b.cfg.Reference, descriptor.Digest)
	if err != nil {
		return fmt.Errorf("registry blob check failed: %w", err)
	}
	if exists {
		b.stats.skipped.Add(1)
		if b.cfg.Metrics != nil {
			b.cfg.Metrics.addSkipped()
		}
		return nil
	}
	b.stats.setStage(stagePushing)
	writer, err := b.cfg.Sink.OpenBlob(ctx, b.cfg.Reference, descriptor.Digest, descriptor.Size)
	if err != nil {
		return fmt.Errorf("registry upload start failed: %w", err)
	}
	// abort releases the registry upload session on every failure path so a
	// broken layer never lingers as a half-written blob.
	abort := func(cause error) error {
		return errors.Join(cause, writer.Abort(ctx))
	}
	content, err := layer.Compressed()
	if err != nil {
		return abort(err)
	}
	buf := make([]byte, 1<<20)
	written, copyErr := io.CopyBuffer(writeCounter{w: writer, stats: b.stats}, content, buf)
	closeErr := content.Close()
	if copyErr != nil {
		return abort(fmt.Errorf("registry upload failed: %w", copyErr))
	}
	if closeErr != nil {
		return abort(closeErr)
	}
	if written != descriptor.Size {
		return abort(fmt.Errorf("layer size mismatch: wrote %d, want %d", written, descriptor.Size))
	}
	if err := writer.Commit(ctx); err != nil {
		return fmt.Errorf("registry upload commit failed: %w", err)
	}
	if b.cfg.Metrics != nil {
		b.cfg.Metrics.addUploaded(uint64(descriptor.Size))
	}
	b.stats.setStage(stageReceiving)
	return nil
}

// applyBoundaryFallback mirrors the local pipeline: close to the OCI layer
// budget the content-defined boundaries give way to fixed ones.
func (b *streamBuilder) applyBoundaryFallback() {
	if !b.cfg.Start.GetDedup() || b.boundaryFallback || len(b.layers) < 110 {
		return
	}
	remaining := int64(b.cfg.Start.GetEstimatedBytes()) - b.plain
	fallback := remaining / 8
	if remaining%8 != 0 {
		fallback++
	}
	if fallback < chunk.MinChunkSize {
		fallback = chunk.MinChunkSize
	}
	b.boundary = chunk.NewFixedBoundary(fallback)
	b.boundaryFallback = true
}

func (b *streamBuilder) publish(ctx context.Context, outcome scanOutcome) (string, error) {
	manifest := b.manifest(outcome.stats)
	chunkTable := &index.ChunkTable{SchemaVersion: index.SchemaVersion, Chunks: b.rows}
	// Mirror the local pipeline: an encrypted backup keeps everything which
	// describes its plaintext inside the sealed private blob.
	var privateBlob []byte
	if private := index.SplitPrivate(manifest, chunkTable); private != nil {
		var buf bytes.Buffer
		if err := index.WritePrivate(&buf, private, b.sealer); err != nil {
			return "", err
		}
		privateBlob = buf.Bytes()
		sum := sha256.Sum256(privateBlob)
		manifest.Private.StoredSha256 = "sha256:" + hex.EncodeToString(sum[:])
	}
	var indexBlob bytes.Buffer
	if err := index.WriteIndex(&indexBlob, &index.Index{
		SchemaVersion: manifest.SchemaVersion, Entries: outcome.entries,
	}, b.sealer); err != nil {
		return "", err
	}
	committer, ok := b.cfg.Sink.(StreamCommitter)
	if !ok {
		return "", errors.New("this server cannot publish streamed backups")
	}
	return committer.CommitStream(ctx, StreamCommit{
		SessionID:   b.cfg.SessionID,
		Reference:   b.cfg.Reference,
		Start:       b.cfg.Start,
		Manifest:    manifest,
		ChunkTable:  chunkTable,
		IndexBlob:   indexBlob.Bytes(),
		PrivateBlob: privateBlob,
		KeyFiles:    b.cfg.Start.GetEncryption().GetKeyFiles(),
		Layers:      b.layers,
		Codec:       b.codec,
		Level:       b.level,
	})
}

func (b *streamBuilder) manifest(stats scanStats) *index.Manifest {
	start := b.cfg.Start
	m := &index.Manifest{
		SchemaVersion: index.SchemaVersion,
		Tool:          index.ToolInfo{Name: "backimage", Version: start.GetToolVersion()},
		CreatedAt:     b.created,
		Sources:       start.GetSources(),
		Host: index.HostInfo{
			Hostname: start.GetHost().GetHostname(),
			OS:       start.GetHost().GetOs(),
			Arch:     start.GetHost().GetArch(),
		},
		Totals: index.Totals{
			Files:       stats.Files,
			Dirs:        stats.Dirs,
			Symlinks:    stats.Symlinks,
			Hardlinks:   stats.Hardlinks,
			Devices:     stats.Devices,
			BytesRaw:    stats.BytesRaw,
			BytesStored: b.stored,
		},
		Archive: index.ArchiveInfo{
			Format: "tar", Compression: b.codec.Name(), CompressionLevel: b.level,
		},
		Chunking: b.chunkingInfo(),
		Layers:   b.layerInfo,
		Index:    index.Ref{Path: "index.json.zst", Encrypted: b.sealer != nil},
	}
	if enc := start.GetEncryption(); enc.GetEnabled() {
		m.Encryption = index.EncryptionInfo{
			Enabled:        true,
			KDF:            "scrypt-age",
			AEAD:           "aes256-gcm",
			NonceMode:      enc.GetNonceMode(),
			KeyFingerprint: enc.GetKeyFingerprint(),
			Recipients:     enc.GetRecipients(),
		}
	}
	return m
}

func (b *streamBuilder) chunkingInfo() index.ChunkingInfo {
	if !b.cfg.Start.GetDedup() {
		return index.ChunkingInfo{
			Strategy: "length", TargetChunkBytes: b.plan.ChunkBytes, Count: b.chunkIdx,
		}
	}
	return index.ChunkingInfo{
		Strategy:         "cdc",
		MinChunkBytes:    b.cdc.Min,
		TargetChunkBytes: b.cdc.Avg,
		MaxChunkBytes:    b.cdc.Max,
		Polynomial:       b.cdc.Polynomial,
		BoundaryFallback: b.boundaryFallback,
		Count:            b.chunkIdx,
	}
}

func (b *streamBuilder) cleanup() {
	if b.spool != nil {
		b.spool.Remove()
		b.spool = nil
	}
	if b.km != nil {
		b.km.Wipe()
		b.km = nil
	}
}

// ociimgLayer is the subset of v1.Layer the upload step needs.
type ociimgLayer interface {
	Compressed() (io.ReadCloser, error)
}

// layerDescriptor reads the identity of a freshly built layer before its
// temporary file is released.
func layerDescriptor(layer v1.Layer, idx uint32) (Layer, error) {
	digest, err := layer.Digest()
	if err != nil {
		return Layer{}, err
	}
	diffID, err := layer.DiffID()
	if err != nil {
		return Layer{}, err
	}
	size, err := layer.Size()
	if err != nil {
		return Layer{}, err
	}
	mediaType, err := layer.MediaType()
	if err != nil {
		return Layer{}, err
	}
	return Layer{
		Index: idx, Size: size, Digest: digest.String(),
		DiffID: diffID.String(), MediaType: string(mediaType),
	}, nil
}

type writeCounter struct {
	w     io.Writer
	stats *streamStats
}

func (c writeCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.stats.uploaded.Add(uint64(n))
	return n, err
}

// spoolFile accumulates the stored bytes of the layer being assembled.
type spoolFile struct {
	path string
	f    *os.File
	size int64
}

func newSpool(dir string, idx int) (*spoolFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("backimage-stream-%06d.blob.tmp", idx))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &spoolFile{path: path, f: f}, nil
}

func (s *spoolFile) Write(p []byte) error {
	n, err := s.f.Write(p)
	s.size += int64(n)
	return err
}

func (s *spoolFile) digest() (string, error) {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, s.f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (s *spoolFile) Close() error { return s.f.Close() }

func (s *spoolFile) Remove() {
	_ = s.f.Close()
	_ = os.Remove(s.path)
}

func dataBlobPath(digest string) string {
	return "backup/data/" + strings.TrimPrefix(digest, "sha256:") + ".blob"
}

func compressionName(start *protocol.StreamStart) string {
	name := strings.TrimSpace(start.GetArchive().GetCompression())
	if name == "" {
		return "zstd"
	}
	return name
}

func cdcFromWire(p *protocol.CDCParams) chunk.CDCParams {
	return chunk.CDCParams{
		Min:        int64(p.GetMin()),
		Avg:        int64(p.GetAvg()),
		Max:        int64(p.GetMax()),
		Polynomial: p.GetPolynomial(),
	}
}

func keyMaterialFromWire(enc *protocol.EncryptionConfig) (*crypt.KeyMaterial, error) {
	km := &crypt.KeyMaterial{
		SchemaVersion: 1,
		DEK:           append([]byte(nil), enc.GetDek()...),
		NonceKey:      append([]byte(nil), enc.GetNonceKey()...),
	}
	if len(km.NonceKey) == 0 {
		// Only the convergent mode reads the nonce key; a random-nonce session
		// still needs a structurally valid key material.
		km.NonceKey = make([]byte, 32)
	}
	if err := km.Validate(); err != nil {
		km.Wipe()
		return nil, err
	}
	return km, nil
}
