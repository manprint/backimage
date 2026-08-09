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

	"github.com/fpierri/backimage/pkg/compress"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/registry"
)

const defaultCacheBytes int64 = 2 << 30

// Source gives random access to the blobs of a backup image.
type Source interface {
	Manifest(context.Context) (*index.Manifest, error)
	ChunkTable(context.Context) (*index.ChunkTable, error)
	KeyFile(context.Context, string) ([]byte, error)
	IndexBlob(context.Context) ([]byte, error)
	Blob(context.Context, int) ([]byte, error)
	Close() error
}

// SourceOptions controls platform selection and the persistent layer cache.
type SourceOptions struct {
	Platform  string
	CacheDir  string
	CacheSize int64
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

	mu      sync.Mutex
	offsets []int64
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
	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("selecting platform %s: %w", p.String(), err)
	}
	return newImageSource(img, opts)
}

// FromOCILayout builds a Source over a local OCI layout directory. ref is
// accepted for API symmetry and future annotation selection.
func FromOCILayout(path, ref string) (Source, error) {
	_ = ref
	lp, err := layout.FromPath(path)
	if err != nil {
		return nil, fmt.Errorf("opening OCI layout: %w", err)
	}
	idx, err := lp.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("reading OCI index: %w", err)
	}
	p, err := sourcePlatform("")
	if err != nil {
		return nil, err
	}
	img, err := imageForPlatform(idx, *p)
	if err != nil {
		return nil, err
	}
	return newImageSource(img, SourceOptions{Platform: p.String()})
}

// FromDaemon builds a Source over an image in the local Docker daemon.
func FromDaemon(ctx context.Context, ref name.Reference) (Source, error) {
	img, err := daemon.Image(ref, daemon.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading daemon image %s: %w", ref.Name(), err)
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
	if opts.CacheSize == 0 {
		opts.CacheSize = defaultCacheBytes
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
		if name != "manifest.json" && name != "chunks.json" && name != "index.json.zst" && name != "keys.age" && name != "keys.pass.age" {
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

func (s *imageSource) ChunkTable(_ context.Context) (*index.ChunkTable, error) {
	s.tableOnce.Do(func() {
		data, err := s.metadata("chunks.json")
		if err != nil {
			s.tableErr = err
			return
		}
		s.table, s.tableErr = index.ReadChunkTable(bytes.NewReader(data))
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
	path, ephemeral, err := s.materialize(ctx, layerIndex, c.P, m.Archive.Compression, expected)
	if err != nil {
		return nil, err
	}
	if ephemeral {
		defer os.Remove(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(s.offsets[i], io.SeekStart); err != nil {
		return nil, err
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

func (s *imageSource) materialize(ctx context.Context, dataLayer int, wanted, codecName string, expected int64) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	layers, err := s.image.Layers()
	if err != nil {
		return "", false, err
	}
	imageLayer := dataLayer + 2
	if imageLayer < 2 || imageLayer >= len(layers) {
		return "", false, fmt.Errorf("data layer %d missing (image has %d layers)", dataLayer, len(layers))
	}
	digest, err := layers[imageLayer].Digest()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(s.cacheDir, 0o700); err != nil {
		return "", false, err
	}
	cachePath := filepath.Join(s.cacheDir, digest.Hex)
	if st, err := os.Stat(cachePath); err == nil && st.Mode().IsRegular() {
		if st.Size() == expected {
			now := time.Now()
			if err := os.Chtimes(cachePath, now, now); err != nil {
				return "", false, err
			}
			return cachePath, false, nil
		}
		if err := os.Remove(cachePath); err != nil {
			return "", false, err
		}
	}
	tmp, err := os.CreateTemp(s.cacheDir, ".layer-*")
	if err != nil {
		return "", false, err
	}
	tmpPath := tmp.Name()
	fail := func(err error) (string, bool, error) {
		tmp.Close()
		os.Remove(tmpPath)
		return "", false, err
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
		return "", false, err
	}
	if s.cacheSize < 0 || st.Size() > s.cacheSize {
		return tmpPath, true, nil
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		return "", false, err
	}
	if err := s.prune(cachePath); err != nil {
		return "", false, err
	}
	return cachePath, false, nil
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

func (*imageSource) Close() error { return nil }

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
