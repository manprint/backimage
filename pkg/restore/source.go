// Package restore reads backimage data lazily from OCI registries, layouts,
// and the local container daemon.
package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/registry"
)

// metadataNames is the closed set of files read from the metadata layer.
var metadataNames = map[string]bool{
	"manifest.json":   true,
	"chunks.json":     true,
	"index.json.zst":  true,
	index.PrivatePath: true,
	"keys.age":        true,
	"keys.pass.age":   true,
}

// Source gives random access to the blobs of a backup image.
type Source interface {
	Manifest(context.Context) (*index.Manifest, error)
	ChunkTable(context.Context) (*index.ChunkTable, error)
	KeyFile(context.Context, string) ([]byte, error)
	IndexBlob(context.Context) ([]byte, error)
	// PrivateBlob returns the sealed confidential metadata of an encrypted
	// backup (index.PrivatePath). It returns os.ErrNotExist for a backup which
	// has none, such as an unencrypted or a schema 1 one.
	PrivateBlob(context.Context) ([]byte, error)
	Blob(context.Context, int) ([]byte, error)
	Close() error
}

// SourceOptions controls platform selection, the persistent layer cache and
// the out-of-band digest the source must match before it is read.
type SourceOptions struct {
	Platform  string
	CacheDir  string
	CacheSize int64
	// ExpectDigest, when set, is compared with the digest the source reports
	// for the object the reference resolved to. The comparison happens before
	// any layer, key file or metadata blob is read, so a caller that holds a
	// trusted digest never hands a secret to the wrong image.
	ExpectDigest ExpectedDigest
}

type imageSource struct {
	image     v1.Image
	platform  string
	cacheDir  string
	cacheSize int64

	metaOnce sync.Once
	meta     map[string][]byte
	metaErr  error

	manifestOnce sync.Once
	manifest     *index.Manifest
	manifestErr  error
	tableOnce    sync.Once
	table        *index.ChunkTable
	tableErr     error

	mu        sync.Mutex
	offsets   []int64
	ephemeral map[int]string
	recent    []int
}

// FromRegistry builds a Source over a remote image reference.
func FromRegistry(ctx context.Context, ref name.Reference, kc registry.Keychain, opts SourceOptions) (Source, error) {
	p, err := sourcePlatform(opts.Platform)
	if err != nil {
		return nil, err
	}
	ropts := []remote.Option{remote.WithContext(ctx), remote.WithPlatform(*p)}
	if kc != nil {
		ropts = append(ropts, remote.WithAuthFromKeychain(kc))
	}
	desc, err := remote.Get(ref, ropts...)
	if err != nil {
		return nil, fmt.Errorf("reading image manifest %s: %w", ref.Name(), err)
	}
	// Before the layers: desc.Digest is what the registry says the reference
	// resolves to, which is the one value a caller can have obtained
	// elsewhere.
	if err := opts.ExpectDigest.matchResolved("registry image "+ref.Name(), desc.Digest); err != nil {
		return nil, err
	}
	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("selecting platform %s: %w", p.String(), err)
	}
	return newImageSource(img, opts)
}

// FromOCILayout builds a Source over a local OCI layout directory. ref is
// accepted for API symmetry and future annotation selection.
//
// Only Platform and ExpectDigest are read from opts: a layout is already on
// disk, so there is nothing to cache.
func FromOCILayout(path, ref string, opts SourceOptions) (Source, error) {
	_ = ref
	lp, err := layout.FromPath(path)
	if err != nil {
		return nil, fmt.Errorf("opening OCI layout: %w", err)
	}
	idx, err := lp.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("reading OCI index: %w", err)
	}
	if err := matchLayoutDigest(opts.ExpectDigest, path, idx); err != nil {
		return nil, err
	}
	p, err := sourcePlatform(opts.Platform)
	if err != nil {
		return nil, err
	}
	img, err := imageForPlatform(idx, *p)
	if err != nil {
		return nil, err
	}
	return newImageSource(img, SourceOptions{Platform: p.String()})
}

// matchLayoutDigest anchors a layout to an expected digest. A layout has no
// registry to ask, so the identities it can be named by are the ones it
// carries: its own top-level index — the digest `backimage backup` reports
// and the digest a push of this layout would produce — and each manifest that
// index advertises, which is what a caller holding a single-platform digest
// has.
//
// The anchor is only worth something if the digest travelled by a different
// route than the layout itself; the documentation says so.
func matchLayoutDigest(want ExpectedDigest, path string, idx v1.ImageIndex) error {
	if !want.Set() {
		return nil
	}
	top, err := idx.Digest()
	if err != nil {
		return fmt.Errorf("reading OCI index: %w", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("reading OCI index: %w", err)
	}
	advertised := make([]v1.Hash, 0, len(im.Manifests)+1)
	advertised = append(advertised, top)
	for _, d := range im.Manifests {
		advertised = append(advertised, d.Digest)
	}
	return want.matchResolved("OCI layout "+path, advertised...)
}

// FromDaemon builds a Source over an image in the local Docker daemon.
//
// Only ExpectDigest is read from opts: the daemon holds one image per
// reference and keeps no layer cache of ours.
func FromDaemon(ctx context.Context, ref name.Reference, opts SourceOptions) (Source, error) {
	img, err := daemon.Image(ref, daemon.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading daemon image %s: %w", ref.Name(), err)
	}
	if opts.ExpectDigest.Set() {
		// The daemon recompresses what it stores, so this digest is the one
		// the local image has now, not the one it had in a registry. The
		// documentation says which anchor is worth what.
		h, err := img.Digest()
		if err != nil {
			return nil, fmt.Errorf("reading daemon image digest %s: %w", ref.Name(), err)
		}
		if err := opts.ExpectDigest.matchResolved("daemon image "+ref.Name(), h); err != nil {
			return nil, err
		}
	}
	return newImageSource(img, SourceOptions{})
}

func sourcePlatform(value string) (*v1.Platform, error) {
	if value == "" {
		value = "linux/amd64"
	}
	p, err := v1.ParsePlatform(value)
	if err != nil {
		return nil, fmt.Errorf("invalid platform %q: %w", value, err)
	}
	return p, nil
}

func imageForPlatform(idx v1.ImageIndex, want v1.Platform) (v1.Image, error) {
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	available := make([]string, 0, len(im.Manifests))
	for _, d := range im.Manifests {
		if d.Platform == nil {
			child, childErr := idx.ImageIndex(d.Digest)
			if childErr == nil {
				if img, selectErr := imageForPlatform(child, want); selectErr == nil {
					return img, nil
				}
			}
			continue
		}
		available = append(available, d.Platform.String())
		if d.Platform.OS == want.OS && d.Platform.Architecture == want.Architecture && (want.Variant == "" || d.Platform.Variant == want.Variant) {
			return idx.Image(d.Digest)
		}
	}
	sort.Strings(available)
	return nil, fmt.Errorf("platform %s not found (available: %v)", want.String(), available)
}

func newImageSource(img v1.Image, opts SourceOptions) (*imageSource, error) {
	if img == nil {
		return nil, errors.New("nil OCI image")
	}
	// A negative size means "never keep a layer on disk"; the CLI documents 0
	// as the way to ask for that, so map it here instead of silently falling
	// back to the default cache.
	if opts.CacheSize == 0 {
		opts.CacheSize = -1
	}
	if opts.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		opts.CacheDir = filepath.Join(base, "backimage", "layers")
	}
	return &imageSource{image: img, platform: opts.Platform, cacheDir: opts.CacheDir, cacheSize: opts.CacheSize}, nil
}

func (s *imageSource) loadMeta() {
	layers, err := s.image.Layers()
	if err != nil {
		s.metaErr = err
		return
	}
	if len(layers) < 2 {
		s.metaErr = fmt.Errorf("backup image has %d layers, want at least executable + metadata", len(layers))
		return
	}
	r, err := layers[1].Uncompressed()
	if err != nil {
		s.metaErr = fmt.Errorf("opening metadata layer: %w", err)
		return
	}
	defer r.Close()
	s.meta = make(map[string][]byte)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.metaErr = fmt.Errorf("metadata tar: %w", err)
			return
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(h.Name, "/"), "backup/")
		if !metadataNames[name] {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil || int64(len(data)) != h.Size {
			s.metaErr = fmt.Errorf("reading metadata %s: %w", name, err)
			return
		}
		s.meta[name] = data
	}
}

func (s *imageSource) metadata(name string) ([]byte, error) {
	s.metaOnce.Do(s.loadMeta)
	if s.metaErr != nil {
		return nil, s.metaErr
	}
	b, ok := s.meta[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, os.ErrNotExist)
	}
	return append([]byte(nil), b...), nil
}

func (s *imageSource) Manifest(_ context.Context) (*index.Manifest, error) {
	s.manifestOnce.Do(func() {
		data, err := s.metadata("manifest.json")
		if err != nil {
			s.manifestErr = err
			return
		}
		s.manifest, s.manifestErr = index.ReadManifest(bytes.NewReader(data))
	})
	return s.manifest, s.manifestErr
}

func (s *imageSource) ChunkTable(ctx context.Context) (*index.ChunkTable, error) {
	s.tableOnce.Do(func() {
		data, err := s.metadata("chunks.json")
		if err != nil {
			s.tableErr = err
			return
		}
		s.table, s.tableErr = index.ReadChunkTable(bytes.NewReader(data))
		if s.tableErr == nil {
			// Both public files are parsed now: check they describe the same
			// backup before any caller allocates from either.
			var m *index.Manifest
			if m, s.tableErr = s.Manifest(ctx); s.tableErr == nil {
				s.tableErr = index.ValidateChunkTable(m, s.table)
			}
		}
		if s.tableErr == nil {
			s.offsets = make([]int64, len(s.table.Chunks))
			byPath := make(map[string]int64)
			for i, c := range s.table.Chunks {
				s.offsets[i] = byPath[c.P]
				byPath[c.P] += c.Sb
			}
		}
	})
	return s.table, s.tableErr
}

func (s *imageSource) KeyFile(_ context.Context, name string) ([]byte, error) {
	if name != "keys.age" && name != "keys.pass.age" {
		return nil, fmt.Errorf("key file %q: %w", name, os.ErrNotExist)
	}
	return s.metadata(name)
}

func (s *imageSource) IndexBlob(_ context.Context) ([]byte, error) {
	return s.metadata("index.json.zst")
}

func (s *imageSource) PrivateBlob(_ context.Context) ([]byte, error) {
	return s.metadata(index.PrivatePath)
}

func (s *imageSource) Blob(ctx context.Context, i int) ([]byte, error) {
	table, err := s.ChunkTable(ctx)
	if err != nil {
		return nil, err
	}
	if i < 0 || i >= len(table.Chunks) {
		return nil, fmt.Errorf("chunk %d out of range", i)
	}
	m, err := s.Manifest(ctx)
	if err != nil {
		return nil, err
	}
	c := table.Chunks[i]
	layerIndex := -1
	var expected int64
	for _, layer := range m.Layers {
		if i >= layer.ChunkFrom && i <= layer.ChunkTo {
			layerIndex = layer.Index
			expected = layer.StoredBytes
			break
		}
	}
	if layerIndex < 0 {
		return nil, fmt.Errorf("chunk %d is not assigned to a data layer", i)
	}
	path, err := s.materialize(ctx, layerIndex, c.P, m.Archive.Compression, expected)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(s.offsets[i], io.SeekStart); err != nil {
		return nil, err
	}
	// The layer is on disk now, so its real size is the authority over what
	// chunks.json declares. Checking it here means an absurd stored size is a
	// format error instead of an allocation.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if s.offsets[i]+c.Sb > fi.Size() {
		return nil, fmt.Errorf("%w: chunk %d declares %d stored bytes at offset %d of a %d byte layer",
			index.ErrBadSchema, i, c.Sb, s.offsets[i], fi.Size())
	}
	if c.Sb > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("chunk %d too large", i)
	}
	out := make([]byte, int(c.Sb))
	if _, err := io.ReadFull(f, out); err != nil {
		return nil, fmt.Errorf("chunk %d truncated in cached layer: %w", i, err)
	}
	return out, nil
}

// ephemeralLayerCap bounds how many uncached layers stay materialised at once.
//
// A full restore reads chunks in order and only ever needs the current layer;
// the selective and the partial paths jump between entries and can alternate
// layers, so a couple of slots turn "rebuild on every jump" back into "rebuild
// once per layer" without letting the temp directory grow without bound.
const ephemeralLayerCap = 4

func (s *imageSource) materialize(ctx context.Context, dataLayer int, wanted, codecName string, expected int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path, ok := s.ephemeral[dataLayer]; ok {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() == expected {
			s.touchEphemeral(dataLayer)
			return path, nil
		}
		s.dropEphemeral(dataLayer)
	}
	layers, err := s.image.Layers()
	if err != nil {
		return "", err
	}
	imageLayer := dataLayer + 2
	if imageLayer < 2 || imageLayer >= len(layers) {
		return "", fmt.Errorf("data layer %d missing (image has %d layers)", dataLayer, len(layers))
	}
	digest, err := layers[imageLayer].Digest()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.cacheDir, 0o700); err != nil {
		return "", err
	}
	cachePath := filepath.Join(s.cacheDir, digest.Hex)
	if st, err := os.Stat(cachePath); err == nil && st.Mode().IsRegular() {
		if st.Size() == expected {
			now := time.Now()
			if err := os.Chtimes(cachePath, now, now); err != nil {
				return "", err
			}
			return cachePath, nil
		}
		if err := os.Remove(cachePath); err != nil {
			return "", err
		}
	}
	tmp, err := os.CreateTemp(s.cacheDir, ".layer-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	fail := func(err error) (string, error) {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	raw, err := layers[imageLayer].Compressed()
	if err != nil {
		return fail(err)
	}
	defer raw.Close()
	codec, err := compress.Get(codecName)
	if err != nil {
		return fail(err)
	}
	decoded, err := codec.NewReader(raw)
	if err != nil {
		return fail(err)
	}
	defer decoded.Close()
	tr := tar.NewReader(&contextReader{ctx: ctx, r: decoded})
	found := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("data layer %d tar: %w", dataLayer, err))
		}
		name := strings.TrimPrefix(h.Name, "/")
		if name != strings.TrimPrefix(wanted, "/") {
			continue
		}
		if h.Size != expected {
			return fail(fmt.Errorf("%s size %d, want %d", wanted, h.Size, expected))
		}
		n, err := io.Copy(tmp, io.LimitReader(tr, expected+1))
		if err != nil {
			return fail(err)
		}
		if n != expected {
			return fail(fmt.Errorf("%s extracted size %d, want %d", wanted, n, expected))
		}
		found = true
		break
	}
	if !found {
		return fail(fmt.Errorf("%s not found in data layer %d", wanted, dataLayer))
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	st, err := tmp.Stat()
	if err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if s.cacheSize < 0 || st.Size() > s.cacheSize {
		// Not cacheable, but still worth keeping for the rest of this layer:
		// releasing it after one chunk meant downloading and decompressing the
		// whole layer again for the next one — 64 GiB of I/O and 64
		// decompressions for a 1 GiB layer of 16 MiB chunks.
		s.keepEphemeral(dataLayer, tmpPath)
		return tmpPath, nil
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if err := s.prune(cachePath); err != nil {
		return "", err
	}
	return cachePath, nil
}

// keepEphemeral registers a materialised layer that the cache policy refused,
// evicting the least recently used one when the slots are full. The caller
// holds s.mu.
func (s *imageSource) keepEphemeral(dataLayer int, path string) {
	if s.ephemeral == nil {
		s.ephemeral = make(map[int]string, ephemeralLayerCap)
	}
	s.ephemeral[dataLayer] = path
	s.recent = append(s.recent, dataLayer)
	for len(s.recent) > ephemeralLayerCap {
		s.dropEphemeral(s.recent[0])
	}
}

// touchEphemeral marks a layer as the most recently used. The caller holds s.mu.
func (s *imageSource) touchEphemeral(dataLayer int) {
	for i, v := range s.recent {
		if v == dataLayer {
			s.recent = append(s.recent[:i], s.recent[i+1:]...)
			break
		}
	}
	s.recent = append(s.recent, dataLayer)
}

// dropEphemeral removes one materialised layer from disk. The caller holds s.mu.
func (s *imageSource) dropEphemeral(dataLayer int) {
	if path, ok := s.ephemeral[dataLayer]; ok {
		os.Remove(path)
		delete(s.ephemeral, dataLayer)
	}
	for i, v := range s.recent {
		if v == dataLayer {
			s.recent = append(s.recent[:i], s.recent[i+1:]...)
			break
		}
	}
}

func (s *imageSource) prune(keep string) error {
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return err
	}
	type cached struct {
		path string
		size int64
		at   time.Time
	}
	files := make([]cached, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".layer-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		p := filepath.Join(s.cacheDir, entry.Name())
		files = append(files, cached{path: p, size: info.Size(), at: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for _, file := range files {
		if total <= s.cacheSize {
			break
		}
		if file.path == keep {
			continue
		}
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total -= file.size
	}
	return nil
}

func (s *imageSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for dataLayer := range s.ephemeral {
		if path, ok := s.ephemeral[dataLayer]; ok {
			os.Remove(path)
		}
	}
	s.ephemeral, s.recent = nil, nil
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
