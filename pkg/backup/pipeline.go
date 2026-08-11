// Package backup implements the end-to-end backup pipeline: archive,
// split, compress, seal, layer spooling, image building and push.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	v1remote "github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/manprint/backimage/pkg/archive"
	"github.com/manprint/backimage/pkg/chunk"
	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/ociimg"
	"github.com/manprint/backimage/pkg/registry"
	backremote "github.com/manprint/backimage/pkg/remote"
	"github.com/manprint/backimage/pkg/restore"
)

// DefaultVersion is embedded in the archive manifest when the CLI does
// not override it.
const DefaultVersion = "0.5.0"

// Config carries every knob of a backup run (mirrors the CLI flags).
type Config struct {
	RootPaths    []string
	Ref          string // full reference, e.g. ghcr.io/me/dumps:latest
	Compression  string
	Level        int   // codec level; 0 = codec default
	MaxLayerSize int64 // target layer bytes, default 1 GiB
	Jobs         int
	Version      string

	Encrypt    bool
	Passphrase func() ([]byte, error) // encrypted with a passphrase
	Recipients []string               // age X25519 public keys
	// Dedup enables CDC, content-defined layer boundaries and convergent
	// encryption. It is deliberately opt-in because it reveals chunk equality.
	Dedup       bool
	DedupParams chunk.CDCParams
	// AgeIdentity lets --dedup reopen a previous keys.age file. Recipients are
	// public keys and therefore cannot perform that operation by themselves.
	AgeIdentity string

	Exclude       []string
	OneFileSystem bool
	NumericOwner  bool
	AllowDegraded bool
	NoMetadata    bool
	Runnable      bool
	Platforms     []string
	TempDir       string
	CheckpointDir string
	Resume        bool
	Keychain      registry.Keychain
	Store         registry.Store
	// RegistryUser selects one login when a registry holds several accounts.
	RegistryUser string
	DryRun       bool
	Output       string // registry|daemon|oci-layout|tar
	OutputPath   string
	Remote       RemoteUploader
	// RemoteStream selects the streaming remote protocol (v2): the server
	// runs the entire pipeline and the client keeps no layers on disk.
	RemoteStream RemoteStreamUploader

	// SelfExtract yields the static restore binary for a GOARCH. When nil,
	// platforms are built without an executable (Runnable must be false).
	SelfExtract func(goarch string) ([]byte, error)

	Progress func(string) // optional diagnostics
	Created  string       // RFC3339; empty = wall clock
}

// RemoteUploader is implemented by remote.Client. Keeping the pipeline
// dependent on this narrow interface makes in-process fault tests cheap.
type RemoteUploader interface {
	Upload(context.Context, backremote.Backup) (backremote.Result, error)
}

// Result reports one backup run (JSON shape from the plan).
type Result struct {
	Ref             string   `json:"ref"`
	Digest          string   `json:"digest"`
	Platforms       []string `json:"platforms"`
	Files           int64    `json:"files"`
	BytesRaw        int64    `json:"bytesRaw"`
	BytesStored     int64    `json:"bytesStored"`
	Layers          int      `json:"layers"`
	Chunks          int      `json:"chunks"`
	Encrypted       bool     `json:"encrypted"`
	Compression     string   `json:"compression"`
	DurationSeconds int64    `json:"durationSeconds"`
	SkippedBlobs    int      `json:"skippedBlobs"`
	SkippedBytes    int64    `json:"skippedBytes"`
	UploadedBytes   int64    `json:"uploadedBytes"`
}

// ErrNoData is returned when the archive stream produced no chunks at all.
var ErrNoData = errors.New("backup: nessun dato prodotto")

// Validate checks flag-level conflicts before any filesystem work.
func Validate(cfg Config) error {
	if len(cfg.RootPaths) == 0 {
		return errors.New("almeno un percorso sorgente e' obbligatorio")
	}
	if cfg.Ref == "" {
		return errors.New("repository obbligatorio (--repo)")
	}
	if _, err := name.ParseReference(cfg.Ref); err != nil {
		return fmt.Errorf("reference %q non valida: %w", cfg.Ref, err)
	}
	switch cfg.Output {
	case "", "registry", "daemon", "oci-layout", "tar":
	default:
		return fmt.Errorf("output %q non supportato (registry|daemon|oci-layout|tar)", cfg.Output)
	}
	if cfg.Output == "oci-layout" || cfg.Output == "tar" {
		if cfg.OutputPath == "" {
			return errors.New("--output-path obbligatorio per " + cfg.Output)
		}
	}
	if cfg.MaxLayerSize < 0 {
		return errors.New("--max-layer-size non puo' essere negativo")
	}
	if cfg.MaxLayerSize > 0 && cfg.MaxLayerSize < chunk.MinChunkSize {
		return fmt.Errorf("--max-layer-size %d inferiore al minimo %d", cfg.MaxLayerSize, chunk.MinChunkSize)
	}
	if cfg.Jobs < 0 {
		return fmt.Errorf("--jobs non puo' essere negativo, got %d", cfg.Jobs)
	}
	if cfg.Encrypt && cfg.Passphrase == nil && len(cfg.Recipients) == 0 {
		return errors.New("cifratura attiva ma nessuna passphrase o destinatario age")
	}
	if cfg.AgeIdentity != "" && !cfg.Dedup {
		return errors.New("--age-identity richiede --dedup")
	}
	if cfg.Output == "daemon" && cfg.OutputPath != "" {
		return errors.New("--output daemon non usa --output-path")
	}
	if cfg.Remote != nil && cfg.RemoteStream != nil {
		return errors.New("only one remote protocol can be selected")
	}
	if cfg.remote() && cfg.Output != "" && cfg.Output != "registry" {
		return errors.New("--remote requires registry output")
	}
	return nil
}

// remote reports whether this run publishes through a remote server, in any
// of the two protocols.
func (c Config) remote() bool { return c.Remote != nil || c.RemoteStream != nil }

// Run executes the pipeline. Order is fixed by the plan document.
func Run(ctx context.Context, cfg Config) (Result, error) {
	start := time.Now()
	var res Result

	if err := Validate(cfg); err != nil {
		return res, err
	}
	if cfg.Compression == "" {
		cfg.Compression = "zstd"
	}
	codec, err := compress.Get(cfg.Compression)
	if err != nil {
		return res, err
	}
	if cfg.Version == "" {
		cfg.Version = DefaultVersion
	}
	if cfg.Jobs <= 0 {
		cfg.Jobs = 3
	}
	if len(cfg.Platforms) == 0 {
		cfg.Platforms = []string{"linux/amd64", "linux/arm64"}
	}
	createdAt := start.UTC()
	if cfg.Created == "" {
		cfg.Created = createdAt.Format(time.RFC3339)
	} else {
		createdAt, err = time.Parse(time.RFC3339, cfg.Created)
		if err != nil {
			return res, fmt.Errorf("created timestamp: %w", err)
		}
	}

	// 0) privilege preflight (01.7): in strict mode a missing capability
	// aborts before any work.
	if !cfg.AllowDegraded {
		caps, perr := archive.PreflightBackup(ctx, cfg.RootPaths)
		if perr != nil {
			return res, fmt.Errorf("preflight: %w", perr)
		}
		for _, c := range caps {
			if !c.Available {
				return res, fmt.Errorf("preflight: %s non disponibile (%s): %s", c.Name, c.Reason, c.Remedy)
			}
		}
	}

	// Resolve credentials and prove repository access before spending time on
	// the estimate/archive. A dry-run is deliberately network-free (GS-05.7).
	// A streaming run still proves registry access up front: the client brokers
	// the tokens the server will use, and a 50 GiB stream must not discover a
	// credential problem at the end.
	if !cfg.DryRun && cfg.Remote == nil && (cfg.Output == "" || cfg.Output == "registry") {
		ref, rerr := name.ParseReference(cfg.Ref)
		if rerr != nil {
			return res, rerr
		}
		kc := cfg.Keychain
		if kc == nil {
			kc = registry.NewKeychainForUser(nil, cfg.Store, cfg.RegistryUser)
		}
		if err := registry.VerifyPushAccess(ctx, ref, kc); err != nil {
			return res, err
		}
	}

	// A previous manifest is only read for an actual direct-registry dedup
	// backup. Dry runs remain strictly network-free.
	var previous *dedupBase
	if cfg.Dedup && !cfg.DryRun && cfg.Remote == nil && (cfg.Output == "" || cfg.Output == "registry" || cfg.RemoteStream != nil) {
		previous, err = findDedupBase(ctx, cfg)
		if err != nil {
			if cfg.Progress != nil {
				cfg.Progress("dedup: impossibile leggere backup precedenti; uso nuovi parametri/chiave") //nolint:misspell // Messaggio CLI italiano.
			}
		} else if previous != nil && cfg.Progress != nil {
			cfg.Progress("dedup: parametri ereditati dal backup " + previous.tag)
		}
	}

	cdcParams, err := chunk.NormalizeCDCParams(cfg.DedupParams)
	if err != nil {
		return res, err
	}
	if cfg.Dedup && previous != nil {
		if inherited, ok := cdcParamsFromManifest(previous.manifest); ok {
			if dedupParamsSpecified(cfg.DedupParams) {
				if !sameCDCParams(cdcParams, inherited) && cfg.Progress != nil {
					cfg.Progress("dedup: parametri CDC diversi dal backup precedente; nessuna deduplica con esso")
				}
			} else {
				cdcParams = inherited
			}
		}
	}

	// 1) light estimate walk (no writes).
	if cfg.Progress != nil {
		cfg.Progress("backup: scansione sorgenti in corso")
	}
	est, err := estimate(ctx, cfg)
	if err != nil {
		return res, fmt.Errorf("stima: %w", err)
	}
	if cfg.Progress != nil {
		cfg.Progress(fmt.Sprintf("sorgenti: %d file, %.1f MiB", est.Files, float64(est.Bytes)/(1<<20)))
		cfg.Progress("backup: pianificazione chunk e layer in corso")
	}

	// 2) plan layers and chunks.
	limits := chunk.DefaultLimits()
	if cfg.MaxLayerSize > 0 {
		limits.TargetLayerBytes = cfg.MaxLayerSize
	}
	plan, err := chunk.PlanLayers(est.Bytes, limits)
	if err != nil {
		return res, err
	}
	if cfg.Dedup {
		plan.ChunkBytes = cdcParams.Avg
	}
	for _, w := range plan.Warnings {
		if cfg.Progress != nil {
			cfg.Progress(w)
		}
	}
	if cfg.Progress != nil {
		cfg.Progress(fmt.Sprintf("backup: piano pronto: %d layer, chunk target %.1f MiB", plan.LayerCount, float64(plan.ChunkBytes)/(1<<20)))
	}

	res = Result{
		Ref:         cfg.Ref,
		Encrypted:   cfg.Encrypt,
		Compression: codec.Name(),
		Files:       est.Files,
		BytesRaw:    est.Bytes,
		Layers:      plan.LayerCount,
		Chunks:      ceilDiv(est.Bytes, plan.ChunkBytes),
		Platforms:   append([]string(nil), cfg.Platforms...),
	}

	if cfg.DryRun {
		return res, nil
	}

	// 3) temp space check. Streaming runs keep no layer on this host, so the
	// requirement collapses to the transport buffers.
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	if cfg.RemoteStream == nil {
		need := int64(cfg.Jobs) * plan.LayerBytes
		if err := checkTempSpace(cfg.TempDir, need); err != nil {
			return res, err
		}
	}

	// Key material is created only after all cheap/preflight checks and never
	// during dry-run. Passphrases are wiped with the DEK on every exit path.
	var km *crypt.KeyMaterial
	var passphrase []byte
	if cfg.Encrypt {
		if cfg.Passphrase != nil {
			passphrase, err = cfg.Passphrase()
			if err != nil {
				return res, err
			}
			defer func() {
				for i := range passphrase {
					passphrase[i] = 0
				}
			}()
		}
		if cfg.Dedup {
			var reused bool
			km, reused = reuseDedupKey(previous, cfg, passphrase)
			if !reused && previous != nil && cfg.Progress != nil {
				cfg.Progress(dedupKeyWarning(previous, cfg))
			}
		}
		if km == nil {
			km, err = crypt.NewKeyMaterial()
			if err != nil {
				return res, err
			}
		}
		defer km.Wipe()
	}

	// 4) streaming section: archive -> split -> compress -> seal -> spool.
	level := cfg.Level
	if level == 0 {
		_, _, def := codec.Levels()
		level = def
	}
	bp := &builder{cfg: cfg, codec: codec, plan: plan, km: km, passphrase: passphrase, level: level, createdAt: createdAt, cdcParams: cdcParams, estimatedRaw: est.Bytes, maxDataLayers: 118}

	// 4-bis) streaming remote: the server owns the pipeline from here on.
	if cfg.RemoteStream != nil {
		res, err = bp.runStream(ctx, est, res)
		if err != nil {
			return res, err
		}
		res.DurationSeconds = elapsedSeconds(start)
		return res, nil
	}

	if err := bp.build(ctx); err != nil {
		if errors.Is(err, ErrNoData) {
			return res, ErrNoData
		}
		return res, err
	}
	if cfg.Progress != nil {
		cfg.Progress("backup: archiviazione/compressione/cifratura completata")
	}
	if err := bp.finalize(); err != nil {
		return res, fmt.Errorf("metadati: %w", err)
	}
	defer bp.cleanup()

	res.Files = bp.stats.Files
	res.BytesRaw = bp.stats.BytesRaw
	res.Chunks = int(bp.chunkIdx)
	res.Layers = len(bp.layers)
	res.BytesStored = bp.storedBytes()

	// 5) images and multi-arch index.
	if cfg.Progress != nil {
		cfg.Progress("backup: preparazione immagini OCI in corso")
	}
	images, idx, err := bp.buildImages()
	if err != nil {
		return res, fmt.Errorf("build immagini: %w", err)
	}
	if cfg.Progress != nil {
		cfg.Progress("backup: immagini OCI pronte")
	}

	// 6) publish.
	if cfg.Remote != nil {
		if cfg.Progress != nil {
			cfg.Progress("backup: upload remoto in corso")
		}
		payload, payloadErr := bp.remotePayload(images)
		if payloadErr != nil {
			return res, fmt.Errorf("remote payload: %w", payloadErr)
		}
		expectedDigest, digestErr := idx.Digest()
		if digestErr != nil {
			return res, fmt.Errorf("local index digest: %w", digestErr)
		}
		payload.ExpectedDigest = expectedDigest.String()
		remoteResult, uploadErr := cfg.Remote.Upload(ctx, payload)
		if uploadErr != nil {
			return res, fmt.Errorf("remote push: %w", uploadErr)
		}
		if remoteResult.Digest != payload.ExpectedDigest {
			return res, fmt.Errorf("remote index digest %s differs from local %s", remoteResult.Digest, payload.ExpectedDigest)
		}
		bp.digest = remoteResult.Digest
		bp.skipped = int(remoteResult.BlobsSkipped)
		if cfg.Progress != nil {
			cfg.Progress("backup: upload remoto completato")
		}
	} else if cfg.Output == "" || cfg.Output == "registry" {
		if cfg.Progress != nil {
			cfg.Progress(fmt.Sprintf("backup: upload registry in corso (%d worker)", cfg.Jobs))
		}
		if err := bp.pushRegistry(ctx, idx, images); err != nil {
			return res, fmt.Errorf("push: %w", err)
		}
		if cfg.Progress != nil {
			cfg.Progress("backup: upload registry completato")
		}
	} else {
		if cfg.Progress != nil {
			cfg.Progress(fmt.Sprintf("backup: scrittura output %s in corso", cfg.Output))
		}
		if err := bp.pushLocal(ctx, idx, images); err != nil {
			return res, fmt.Errorf("output %s: %w", cfg.Output, err)
		}
		if cfg.Progress != nil {
			cfg.Progress("backup: scrittura output completata")
		}
	}

	res.Digest = bp.digest
	res.SkippedBlobs = bp.skipped
	res.SkippedBytes = bp.skippedBytes
	res.UploadedBytes = bp.uploadedBytes
	if cfg.Dedup && cfg.Progress != nil && (cfg.Output == "" || cfg.Output == "registry") {
		cfg.Progress(fmt.Sprintf("dedup: %d blob gia' presenti (%d byte risparmiati)", res.SkippedBlobs, res.SkippedBytes))
		cfg.Progress(fmt.Sprintf("upload: %d byte", res.UploadedBytes))
	}
	res.DurationSeconds = elapsedSeconds(start)
	return res, nil
}

func elapsedSeconds(start time.Time) int64 {
	seconds := int64(time.Since(start).Round(time.Second) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// Estimate is the result of the light, non-modifying walk.
type Estimate struct {
	Files int64
	Bytes int64
}

// estimate walks every root counting regular files and raw bytes. It never
// writes and does not follow symlinks (matching the archiver).
func estimate(ctx context.Context, cfg Config) (Estimate, error) {
	var est Estimate
	for _, root := range cfg.RootPaths {
		n, sz, err := walkEstimate(ctx, root, cfg.AllowDegraded)
		if err != nil {
			return est, err
		}
		est.Files += n
		est.Bytes += sz
	}
	return est, nil
}

func walkEstimate(ctx context.Context, root string, degraded bool) (int64, int64, error) {
	fi, err := os.Lstat(root)
	if err != nil {
		return 0, 0, err
	}
	if !fi.IsDir() {
		return 1, fi.Size(), nil
	}
	var n, sz int64
	err = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if degraded {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			n++
			sz += info.Size()
		}
		return nil
	})
	return n, sz, err
}

func ceilDiv(a, b int64) int {
	if b <= 0 || a <= 0 {
		return 0
	}
	return int((a + b - 1) / b)
}

type dedupBase struct {
	manifest *index.Manifest
	keyFiles map[string][]byte
	tag      string
}

// findDedupBase selects the newest readable backimage tag in the target
// repository. A failed listing is non-fatal to a backup: the first backup of
// a repository necessarily has nothing to inherit.
func findDedupBase(ctx context.Context, cfg Config) (*dedupBase, error) {
	ref, err := name.ParseReference(cfg.Ref)
	if err != nil {
		return nil, err
	}
	kc := cfg.Keychain
	if kc == nil {
		kc = registry.NewKeychainForUser(nil, cfg.Store, cfg.RegistryUser)
	}
	opts := []v1remote.Option{v1remote.WithContext(ctx), v1remote.WithAuthFromKeychain(kc)}
	tags, err := v1remote.List(ref.Context(), opts...)
	if err != nil {
		return nil, err
	}
	sort.Strings(tags)
	var best *dedupBase
	for _, tag := range tags {
		candidate, tagErr := name.NewTag(ref.Context().Name() + ":" + tag)
		if tagErr != nil {
			continue
		}
		src, srcErr := restore.FromRegistry(ctx, candidate, kc, restore.SourceOptions{CacheDir: cfg.TempDir, CacheSize: 1 << 20})
		if srcErr != nil {
			continue
		}
		m, manifestErr := src.Manifest(ctx)
		if manifestErr != nil {
			_ = src.Close()
			continue
		}
		if best != nil && (!m.CreatedAt.After(best.manifest.CreatedAt) && (m.CreatedAt != best.manifest.CreatedAt || tag <= best.tag)) {
			_ = src.Close()
			continue
		}
		files := map[string][]byte{}
		for _, keyName := range []string{"keys.pass.age", "keys.age"} {
			data, keyErr := src.KeyFile(ctx, keyName)
			if keyErr == nil {
				files[keyName] = append([]byte(nil), data...)
			}
		}
		_ = src.Close()
		best = &dedupBase{manifest: m, keyFiles: files, tag: tag}
	}
	return best, nil
}

func cdcParamsFromManifest(m *index.Manifest) (chunk.CDCParams, bool) {
	if m == nil || m.Chunking.Strategy != "cdc" {
		return chunk.CDCParams{}, false
	}
	p, err := chunk.NormalizeCDCParams(chunk.CDCParams{
		Min:        m.Chunking.MinChunkBytes,
		Avg:        m.Chunking.TargetChunkBytes,
		Max:        m.Chunking.MaxChunkBytes,
		Polynomial: m.Chunking.Polynomial,
	})
	if err != nil {
		return chunk.CDCParams{}, false
	}
	return p, true
}

func dedupParamsSpecified(p chunk.CDCParams) bool {
	return p.Min != 0 || p.Avg != 0 || p.Max != 0 || p.Polynomial != 0
}

func sameCDCParams(a, b chunk.CDCParams) bool {
	return a.Min == b.Min && a.Avg == b.Avg && a.Max == b.Max && a.Polynomial == b.Polynomial
}

func reuseDedupKey(previous *dedupBase, cfg Config, passphrase []byte) (*crypt.KeyMaterial, bool) {
	if previous == nil || !previous.manifest.Encryption.Enabled || previous.manifest.Encryption.NonceMode != "convergent" {
		return nil, false
	}
	identity := crypt.Identity{Passphrase: passphrase, AgeKeyFile: cfg.AgeIdentity}
	for _, keyName := range []string{"keys.pass.age", "keys.age"} {
		data := previous.keyFiles[keyName]
		if len(data) == 0 {
			continue
		}
		km, err := crypt.UnwrapKeys(bytes.NewReader(data), identity)
		if err == nil {
			return km, true
		}
	}
	return nil, false
}

func dedupKeyWarning(previous *dedupBase, cfg Config) string {
	if previous == nil {
		return ""
	}
	if !previous.manifest.Encryption.Enabled || previous.manifest.Encryption.NonceMode != "convergent" {
		return "dedup: modalita' nonce precedente diversa; generata una nuova chiave"
	}
	if cfg.Passphrase != nil {
		return "dedup: passphrase diversa: la dedup con i backup precedenti non sara' possibile" //nolint:misspell // Messaggio CLI italiano richiesto dal contratto della fase 10.
	}
	return "dedup: serve --age-identity per riusare la chiave precedente"
}

// Statfs is overridable for tests.
var statfs = func(dir string) (free int64, err error) {
	return platformFreeSpace(dir)
}

func checkTempSpace(dir string, need int64) error {
	free, err := statfs(dir)
	if err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	if free < need {
		return fmt.Errorf("spazio temporaneo insufficiente in %s: servono %d GiB, disponibili %d GiB; usare --temp-dir o ridurre --max-layer-size",
			dir, ceilDiv(need, 1<<30), ceilDiv(free, 1<<30))
	}
	return nil
}

// builder is the streaming state of one backup run.
type builder struct {
	cfg        Config
	codec      compress.Codec
	plan       chunk.Plan
	km         *crypt.KeyMaterial
	passphrase []byte
	sealer     crypt.Sealer

	spool         *spoolFile
	rows          []index.Chunk
	layers        []index.LayerInfo
	data          []v1.Layer
	images        map[string]v1.Image
	idx           v1.ImageIndex
	digest        string
	skipped       int
	skippedBytes  int64
	uploadedBytes int64

	chunkIdx         uint32
	blobIdx          int
	level            int
	cdcParams        chunk.CDCParams
	boundary         chunk.LayerBoundary
	boundaryFallback bool
	estimatedRaw     int64
	plainBytes       int64
	maxDataLayers    int
	stats            archive.Stats
	entries          []archive.Entry
	progressBytes    int64
	progressAt       time.Time
	manifest         *index.Manifest
	manifestBytes    []byte
	chunkTable       *index.ChunkTable
	indexBlob        []byte
	privateBlob      []byte
	tempFiles        []string
	cleaned          bool
	createdAt        time.Time
}

// spoolFile accumulates the stored bytes of one blob on disk.
type spoolFile struct {
	path string
	f    *os.File
	size int64
}

func newSpool(dir string, idx int) (*spoolFile, error) {
	path := filepath.Join(dir, fmt.Sprintf("backimage-layer-%06d.blob.tmp", idx))
	f, err := os.Create(path)
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

func (s *spoolFile) Close() error { return s.f.Close() }

func (s *spoolFile) Remove() {
	_ = s.f.Close()
	_ = os.Remove(s.path)
}

// build runs the streaming archive/split/compress/seal section.
func (b *builder) build(ctx context.Context) error {
	b.reportBuildProgress(true)
	pr, pw := io.Pipe()
	awErr := make(chan error, 1)
	go func() {
		defer pw.Close()
		aw := archive.NewWriter(pw, archive.Options{
			Strict:         !b.cfg.AllowDegraded,
			OneFileSystem:  b.cfg.OneFileSystem,
			Excludes:       b.cfg.Exclude,
			NumericOwner:   b.cfg.NumericOwner,
			PreserveACLs:   true,
			PreserveXattrs: true,
		})
		for _, r := range b.cfg.RootPaths {
			if err := aw.AddRoot(ctx, r); err != nil {
				_ = pw.CloseWithError(err)
				awErr <- err
				return
			}
		}
		st, err := aw.Close()
		if err != nil {
			_ = pw.CloseWithError(err)
			awErr <- err
			return
		}
		b.stats = st
		b.entries = aw.Entries()
		awErr <- nil
	}()

	var sp chunk.Splitter
	if b.cfg.Dedup {
		sp = chunk.NewCDC(pr, b.cdcParams)
		b.boundary = chunk.NewContentBoundary(b.plan.LayerBytes, b.plan.LayerBytes/4, safeLayerMaximum(b.plan.LayerBytes))
	} else {
		var err error
		sp, err = chunk.NewFixed(pr, b.plan.ChunkBytes)
		if err != nil {
			return err
		}
		b.boundary = chunk.NewFixedBoundary(b.plan.LayerBytes)
	}
	if b.km != nil {
		mode := crypt.NonceRandom
		if b.cfg.Dedup {
			mode = crypt.NonceConvergent
		}
		se, err := crypt.NewSealer(b.km, mode)
		if err != nil {
			return err
		}
		b.sealer = se
	}

	for {
		ck, err := sp.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("split: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		stored, err := b.storeChunk(ck)
		if err != nil {
			return err
		}
		b.rows = append(b.rows, index.Chunk{
			I:  int(ck.Index),
			P:  b.blobPath(),
			Ps: "sha256:" + hex.EncodeToString(ck.PlainSHA[:]),
			Sb: int64(len(stored)),
			Pb: ck.PlainBytes,
			Ss: "sha256:" + hex.EncodeToString(sha256Of(stored)),
		})
		b.chunkIdx++
		b.plainBytes += ck.PlainBytes
		b.reportBuildProgress(false)
		if b.shouldRollLayer(sha256.Sum256(stored)) {
			if err := b.rollLayer(); err != nil {
				return err
			}
		}
	}

	if err := <-awErr; err != nil {
		return fmt.Errorf("archivio: %w", err)
	}
	if b.chunkIdx == 0 {
		return ErrNoData
	}
	if err := b.rollLayer(); err != nil {
		return err
	}
	b.reportBuildProgress(true)
	return nil
}

// reportBuildProgress emits sparse updates for the expensive
// archive/compress/encrypt stream. The estimate is the source byte count, so
// tar headers and metadata can make the stream slightly larger.
func (b *builder) reportBuildProgress(force bool) {
	if b.cfg.Progress == nil || b.estimatedRaw <= 0 {
		return
	}
	now := time.Now()
	const minBytes = 16 << 20
	if !force && !b.progressAt.IsZero() && now.Sub(b.progressAt) < 2*time.Second && b.plainBytes-b.progressBytes < minBytes {
		return
	}
	percent := float64(b.plainBytes) * 100 / float64(b.estimatedRaw)
	if percent > 100 {
		percent = 100
	}
	b.cfg.Progress(fmt.Sprintf("dump: archiviazione/compressione/cifratura %.0f%% (%.1f/%.1f MiB, %d chunk)",
		percent, float64(b.plainBytes)/(1<<20), float64(b.estimatedRaw)/(1<<20), b.chunkIdx))
	b.progressBytes = b.plainBytes
	b.progressAt = now
}

func safeLayerMaximum(target int64) int64 {
	if target <= 0 || target > int64(^uint64(0)>>1)/4 {
		return int64(^uint64(0) >> 1)
	}
	return target * 4
}

func (b *builder) shouldRollLayer(chunkDigest [32]byte) bool {
	if b.spool == nil || b.boundary == nil {
		return false
	}
	maxLayers := b.maxDataLayers
	if maxLayers <= 0 {
		maxLayers = 118
	}
	// Keep a final, unconstrained layer available. This is the hard guard
	// behind the probabilistic content boundaries.
	if len(b.layers) >= maxLayers-1 {
		return false
	}
	return b.boundary.ShouldClose(b.spool.size, chunkDigest)
}

// storeChunk compresses (and optionally seals) one chunk into the spool.
func (b *builder) storeChunk(ck *chunk.Chunk) ([]byte, error) {
	var cbuf bytes.Buffer
	cw, err := b.codec.NewWriter(&cbuf, b.level)
	if err != nil {
		return nil, err
	}
	if _, err := cw.Write(ck.Data); err != nil {
		return nil, err
	}
	if err := cw.Close(); err != nil {
		return nil, err
	}
	compressed := cbuf.Bytes()

	var stored []byte
	if b.sealer != nil {
		stored, err = b.sealer.Seal(nil, uint32(ck.Index), b.codec, compressed, ck.PlainSHA)
		if err != nil {
			return nil, fmt.Errorf("seal chunk %d: %w", ck.Index, err)
		}
	} else {
		stored = compressed
	}

	if b.spool == nil {
		s, err := newSpool(b.cfg.TempDir, b.blobIdx)
		if err != nil {
			return nil, err
		}
		b.spool = s
	}
	if err := b.spool.Write(stored); err != nil {
		return nil, err
	}
	return stored, nil
}

// rollLayer turns the current spool into an in-memory v1.Layer.
func (b *builder) rollLayer() error {
	if b.spool == nil {
		return nil
	}
	if b.maxDataLayers > 0 && len(b.layers) >= b.maxDataLayers {
		return fmt.Errorf("dedup layer limit exceeded: %d", b.maxDataLayers)
	}
	digest, err := streamDigest(b.spool)
	if err != nil {
		b.spool.Remove()
		return err
	}
	if err := b.spool.Close(); err != nil {
		return err
	}

	from := 0
	if len(b.layers) > 0 {
		from = b.layers[len(b.layers)-1].ChunkTo + 1
	}
	to := int(b.chunkIdx) - 1
	// A sequential filename would make an otherwise identical OCI layer differ
	// whenever an earlier probabilistic boundary changes its ordinal. Name the
	// file from its stored content instead; rows are rewritten before their
	// chunk table is serialised, and the per-layer ChunkFrom/ChunkTo mapping
	// still identifies exactly where to read it during restore.
	dataPath := dataBlobPath(digest)
	for i := from; i <= to && i < len(b.rows); i++ {
		b.rows[i].P = dataPath
	}

	lf := ociimg.LayerFile{
		Path: "/" + dataPath,
		Mode: 0o644,
		Size: b.spool.size,
		Open: func() (io.ReadCloser, error) {
			return os.Open(b.spool.path)
		},
	}
	l, err := ociimg.NewFileLayer([]ociimg.LayerFile{lf}, b.codec, b.level, b.cfg.TempDir)
	if err != nil {
		b.spool.Remove()
		return err
	}
	b.data = append(b.data, l)
	b.layers = append(b.layers, index.LayerInfo{
		Index:       len(b.layers),
		Digest:      digest,
		ChunkFrom:   from,
		ChunkTo:     to,
		StoredBytes: b.spool.size,
	})
	b.spool.Remove()
	b.blobIdx++
	b.spool = nil
	if b.cfg.Dedup && !b.boundaryFallback && len(b.layers) >= 110 {
		remaining := b.estimatedRaw - b.plainBytes
		fallbackSize := remaining / 8
		if remaining%8 != 0 {
			fallbackSize++
		}
		if fallbackSize < chunk.MinChunkSize {
			fallbackSize = chunk.MinChunkSize
		}
		b.boundary = chunk.NewFixedBoundary(fallbackSize)
		b.boundaryFallback = true
		if b.cfg.Progress != nil {
			b.cfg.Progress("dedup: raggiunti 110 layer; confini fissi per rispettare il limite OCI")
		}
	}
	return nil
}

func (b *builder) blobPath() string { return fmt.Sprintf("backup/data/%06d.blob", b.blobIdx) }

func dataBlobPath(digest string) string {
	return "backup/data/" + strings.TrimPrefix(digest, "sha256:") + ".blob"
}

func streamDigest(s *spoolFile) (string, error) {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, s.f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func sha256Of(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// finalize assembles manifest, chunk table and index blob.
func (b *builder) finalize() error {
	m := &index.Manifest{
		SchemaVersion: 1,
		Tool: index.ToolInfo{
			Name:    "backimage",
			Version: b.cfg.Version,
		},
		CreatedAt: b.createdAt.UTC(),
		Sources:   b.cfg.RootPaths,
		Host: index.HostInfo{
			Hostname: hostname(),
			OS:       runtime.GOOS,
			Arch:     runtime.GOARCH,
		},
		Totals: index.Totals{
			Files:       b.stats.Files,
			Dirs:        b.stats.Dirs,
			Symlinks:    b.stats.Symlinks,
			Hardlinks:   b.stats.Hardlinks,
			Devices:     b.stats.Devices + b.stats.Fifos,
			BytesRaw:    b.stats.BytesRaw,
			BytesStored: b.storedBytes(),
		},
		Archive: index.ArchiveInfo{
			Format:           "tar",
			Compression:      b.codec.Name(),
			CompressionLevel: b.level,
		},
		Chunking: b.chunkingInfo(),
		Layers:   b.layers,
		Index:    index.Ref{Path: "index.json.zst", Encrypted: b.cfg.Encrypt},
	}
	if b.cfg.Encrypt {
		m.Encryption = index.EncryptionInfo{
			Enabled:        true,
			KDF:            "scrypt-age",
			AEAD:           "aes256-gcm",
			NonceMode:      nonceMode(b.cfg.Dedup),
			KeyFingerprint: keyFingerprint(b.km),
			Recipients:     b.cfg.Recipients,
		}
	}
	if b.cfg.NoMetadata {
		m.Sources = nil
		m.Host = index.HostInfo{}
	}

	b.chunkTable = &index.ChunkTable{SchemaVersion: index.SchemaVersion, Chunks: b.rows}

	// An encrypted backup publishes no description of its plaintext: source
	// paths, host, totals and the per-chunk plaintext digests and sizes move
	// into the sealed private blob, which only the backup key can open.
	if private := index.SplitPrivate(m, b.chunkTable); private != nil {
		var pb bytes.Buffer
		if err := index.WritePrivate(&pb, private, b.sealer); err != nil {
			return err
		}
		b.privateBlob = pb.Bytes()
		m.Private.StoredSha256 = "sha256:" + hex.EncodeToString(sha256Of(b.privateBlob))
	}
	b.manifest = m
	b.manifestBytes = manifestJSON(m)

	ict := &index.Index{
		SchemaVersion: m.SchemaVersion,
		Entries:       archiveToIndex(b.entries),
	}
	var ib bytes.Buffer
	if err := index.WriteIndex(&ib, ict, b.sealer); err != nil {
		return err
	}
	b.indexBlob = ib.Bytes()
	return nil
}

func (b *builder) chunkingInfo() index.ChunkingInfo {
	if !b.cfg.Dedup {
		return index.ChunkingInfo{
			Strategy:         "length",
			TargetChunkBytes: b.plan.ChunkBytes,
			Count:            int(b.chunkIdx),
		}
	}
	return index.ChunkingInfo{
		Strategy:         "cdc",
		MinChunkBytes:    b.cdcParams.Min,
		TargetChunkBytes: b.cdcParams.Avg,
		MaxChunkBytes:    b.cdcParams.Max,
		Polynomial:       b.cdcParams.Polynomial,
		BoundaryFallback: b.boundaryFallback,
		Count:            int(b.chunkIdx),
	}
}

func nonceMode(dedup bool) string {
	if dedup {
		return "convergent"
	}
	return "random"
}

func keyFingerprint(km *crypt.KeyMaterial) string {
	if km == nil || len(km.DEK) == 0 {
		return ""
	}
	h := sha256.Sum256(km.DEK)
	return hex.EncodeToString(h[:8])
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func manifestJSON(m *index.Manifest) []byte {
	var buf bytes.Buffer
	if err := index.WriteManifest(&buf, m); err != nil {
		return nil
	}
	return buf.Bytes()
}

// archiveToIndex converts tar entries into index FileEntries.
func archiveToIndex(entries []archive.Entry) []index.FileEntry {
	out := make([]index.FileEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, index.FileEntry{
			Path:       e.Path,
			Type:       typeCode(e.Type),
			Size:       e.Size,
			Mode:       index.FormatMode(archiveMode(e.Mode)),
			UID:        e.UID,
			GID:        e.GID,
			UName:      e.Uname,
			GName:      e.Gname,
			MTime:      e.ModTime,
			LinkTarget: e.LinkTarget,
			TarOffset:  e.TarOffset,
			SHA256:     e.SHA256,
		})
	}
	return out
}

func archiveMode(mode os.FileMode) uint32 {
	m := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		m |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		m |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		m |= 0o1000
	}
	return m
}

func typeCode(t archive.EntryType) string {
	switch t {
	case archive.TypeRegular:
		return index.TypeRegular
	case archive.TypeDir:
		return index.TypeDir
	case archive.TypeSymlink:
		return index.TypeSymlink
	case archive.TypeHardlink:
		return index.TypeHardlink
	case archive.TypeCharDevice:
		return index.TypeChar
	case archive.TypeBlockDevice:
		return index.TypeBlock
	case archive.TypeFifo:
		return index.TypeFifo
	}
	return index.TypeRegular
}

func (b *builder) storedBytes() int64 {
	var n int64
	for _, r := range b.rows {
		n += r.Sb
	}
	return n
}

// buildImages assembles the per-platform images and the multi-arch index.
func (b *builder) buildImages() (map[string]v1.Image, v1.ImageIndex, error) {
	created := b.cfg.Created
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}

	var keyFiles map[string][]byte
	if b.cfg.Encrypt {
		var err error
		keyFiles, err = b.buildKeyFiles()
		if err != nil {
			return nil, nil, err
		}
	}

	byPlatform := map[string]v1.Image{}
	built := make([]ociimg.BuiltImage, 0, len(b.platforms()))
	for _, plat := range b.platforms() {
		var exe []byte
		var err error
		if b.cfg.SelfExtract != nil {
			exe, err = b.cfg.SelfExtract(plat.Architecture)
			if err != nil {
				return nil, nil, fmt.Errorf("self-extract %s: %w", plat.Architecture, err)
			}
		}
		opts := ociimg.BuildOptions{
			Platform:    plat,
			SelfExtract: exe,
			Runnable:    b.cfg.Runnable,
			Manifest:    b.manifest,
			ChunkTable:  b.chunkTable,
			IndexBlob:   b.indexBlob,
			PrivateBlob: b.privateBlob,
			KeyFiles:    keyFiles,
			DataLayers:  b.data,
			Codec:       b.codec,
			Created:     created,
		}
		img, err := ociimg.BuildImage(opts)
		if err != nil {
			return nil, nil, err
		}
		byPlatform[platString(plat)] = img
		built = append(built, ociimg.BuiltImage{Platform: plat, Image: img})
	}
	idx, err := ociimg.BuildIndex(built)
	if err != nil {
		return nil, nil, err
	}
	b.images = byPlatform
	b.idx = idx

	raw, err := idx.RawManifest()
	if err != nil {
		return nil, nil, err
	}
	b.digest = "sha256:" + hex.EncodeToString(sha256Of(raw))
	return byPlatform, idx, nil
}

func (b *builder) platforms() []v1.Platform {
	out := make([]v1.Platform, 0, len(b.cfg.Platforms))
	for _, s := range b.cfg.Platforms {
		parts := strings.Split(s, "/")
		p := v1.Platform{OS: "linux"}
		switch len(parts) {
		case 1:
			p.Architecture = parts[0]
		case 2:
			p.OS, p.Architecture = parts[0], parts[1]
		case 3:
			p.OS = parts[0]
			p.Architecture = parts[1]
			p.Variant = parts[2]
		}
		out = append(out, p)
	}
	return out
}

func platString(p v1.Platform) string {
	if p.Variant != "" {
		return p.OS + "/" + p.Architecture + "/" + p.Variant
	}
	return p.OS + "/" + p.Architecture
}

// buildKeyFiles wraps the KeyMaterial for the configured recipients.
func (b *builder) buildKeyFiles() (map[string][]byte, error) {
	if b.km == nil {
		return map[string][]byte{}, nil
	}
	return wrapKeyFiles(b.km, b.passphrase, b.cfg.Recipients)
}

// cleanup removes any leftover temp files.
func (b *builder) cleanup() {
	if b.cleaned {
		return
	}
	b.cleaned = true
	if b.spool != nil {
		b.spool.Remove()
	}
	for _, p := range b.tempFiles {
		_ = os.Remove(p)
	}
	for _, layer := range b.data {
		ociimg.RemoveLayer(layer)
	}
	b.data = nil
	b.tempFiles = nil
}

// pushRegistry publishes through the checkpoint-aware push.
func (b *builder) pushRegistry(ctx context.Context, idx v1.ImageIndex, images map[string]v1.Image) error {
	opts := registry.PushOptions{
		Jobs:       b.cfg.Jobs,
		ChunkSize:  8 << 20,
		MaxRetries: 5,
	}
	if b.cfg.Resume {
		opts.Checkpoint = registry.NewCheckpointStore(b.cfg.CheckpointDir)
		opts.ID = checkpointID(b.cfg)
		opts.Manifest = b.manifestBytes
	}
	pch := make(chan registry.Progress, 8)
	opts.Progress = pch
	done := make(chan struct{})
	go func() {
		defer close(done)
		resumingReported := false
		completedBlobs := 0
		completedBytes := int64(0)
		lastReport := time.Time{}
		report := func(force bool) {
			if b.cfg.Progress == nil {
				return
			}
			now := time.Now()
			if !force && !lastReport.IsZero() && now.Sub(lastReport) < 2*time.Second {
				return
			}
			b.cfg.Progress(fmt.Sprintf("upload: %d blob completati (%.1f MiB)", completedBlobs, float64(completedBytes)/(1<<20)))
			lastReport = now
		}
		for pr := range pch {
			switch pr.Event {
			case "checking":
				if b.cfg.Progress != nil {
					b.cfg.Progress(fmt.Sprintf("upload: verifica presenza blob %s (layer %d)", pr.Blob, pr.Layer))
				}
			case "uploading":
				if b.cfg.Progress != nil {
					b.cfg.Progress(fmt.Sprintf("upload: invio blob %s (layer %d, %.1f MiB)", pr.Blob, pr.Layer, float64(pr.Total)/(1<<20)))
				}
			case "manifests":
				if b.cfg.Progress != nil {
					b.cfg.Progress("upload: pubblicazione manifest e indice OCI")
				}
			case "published":
				if b.cfg.Progress != nil {
					b.cfg.Progress("upload: manifest e indice OCI pubblicati")
				}
			default:
				completedBlobs++
				if pr.Skipped || pr.Event == "skipped" {
					b.skipped++
					b.skippedBytes += pr.Total
				} else {
					b.uploadedBytes += pr.Total
				}
				completedBytes += pr.Total
				if b.cfg.Progress != nil {
					status := "completato"
					if pr.Skipped || pr.Event == "skipped" {
						status = "gia' presente, saltato"
					}
					b.cfg.Progress(fmt.Sprintf("upload: blob %s %s (%d completati)", pr.Blob, status, completedBlobs))
				}
				report(false)
			}
			if pr.FromCheckpoint && !resumingReported && b.cfg.Progress != nil {
				// Keep the stable marker used by operators and E2E checks; the
				// surrounding progress logger adds the timestamp.
				b.cfg.Progress("resuming from checkpoint")
				resumingReported = true
			}
		}
		report(true)
	}()
	kc := b.cfg.Keychain
	if kc == nil {
		kc = registry.NewKeychainForUser(nil, b.cfg.Store, b.cfg.RegistryUser)
	}
	ref, err := name.ParseReference(b.cfg.Ref)
	if err != nil {
		close(pch)
		return err
	}
	err = registry.Push(ctx, ref, images, idx, kc, opts)
	close(pch)
	<-done
	return err
}

func checkpointID(cfg Config) string {
	parts := append([]string{cfg.Ref}, cfg.RootPaths...)
	sort.Strings(parts)
	parts = append(parts,
		cfg.Compression,
		fmt.Sprintf("level=%d", cfg.Level),
		fmt.Sprintf("layer=%d", cfg.MaxLayerSize),
		fmt.Sprintf("encrypt=%t", cfg.Encrypt),
		"version="+cfg.Version,
	)
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:12])
}

// pushLocal writes to daemon / oci-layout / tar.
func (b *builder) pushLocal(ctx context.Context, idx v1.ImageIndex, images map[string]v1.Image) error {
	opts := ociimg.WriterOptions{Images: images, Runtime: runtimePlatform()}
	switch b.cfg.Output {
	case "daemon":
		w, err := ociimg.NewWriter(ociimg.TargetDaemon, "", opts)
		if err != nil {
			return err
		}
		return w.Write(ctx, refFor(b.cfg.Ref), idx, nil)
	case "oci-layout", "tar":
		t := ociimg.TargetOCILayout
		if b.cfg.Output == "tar" {
			t = ociimg.TargetTar
		}
		w, err := ociimg.NewWriter(t, b.cfg.OutputPath, opts)
		if err != nil {
			return err
		}
		return w.Write(ctx, refFor(b.cfg.Ref), idx, nil)
	}
	return nil
}

func runtimePlatform() v1.Platform {
	return v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
}

func refFor(s string) name.Reference {
	r, err := name.ParseReference(s)
	if err != nil {
		// Validate ran before; this cannot fail.
		panic(err)
	}
	return r
}

var _ = json.Marshal // keep encoding/json linked for API stability
