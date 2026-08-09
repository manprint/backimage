// Package ociimg assembles the OCI image that carries a backup: one layer
// holding all shared data (chunks, manifest, compressed index), an
// architecture-specific entrypoint layer (/backimage) and multi-platform
// image index support.
package ociimg

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/fpierri/backimage/pkg/compress"
)

// LayerFile is one file to be placed inside a layer tar.
type LayerFile struct {
	Path string // absolute path inside the image, e.g. "/backup/data/000000.blob"
	Mode int64  // 0644 or 0755
	Size int64  // must match the number of bytes Open returns
	Open func() (io.ReadCloser, error)
}

// BuildLayerTar writes a deterministic tar containing files, in the order
// given. Ownership is forced to 0:0, modification time to the Unix epoch and
// no PAX records are emitted: two calls with identical inputs produce
// identical bytes. Intermediate directories are emitted explicitly, once,
// in lexicographic order before the files.
func BuildLayerTar(w io.Writer, files []LayerFile) error {
	dirs := collectDirs(files)
	now := time.Unix(0, 0).UTC()
	tw := tar.NewWriter(w)

	for _, d := range dirs {
		hdr := &tar.Header{
			Name:     d,
			Mode:     0o755,
			Size:     0,
			Typeflag: tar.TypeDir,
			Uid:      0, Gid: 0,
			ModTime: now,
		}
		hdr.Format = formatFor(hdr.Name)
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("dir %s: %w", d, err)
		}
	}
	for _, f := range files {
		name := path.Clean("/" + strings.TrimPrefix(f.Path, "/"))
		hdr := &tar.Header{
			Name:     name,
			Mode:     f.Mode,
			Size:     f.Size,
			Typeflag: tar.TypeReg,
			Uid:      0, Gid: 0,
			ModTime: now,
		}
		hdr.Format = formatFor(name)
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		if f.Size > 0 {
			r, err := f.Open()
			if err != nil {
				return fmt.Errorf("opening %s: %w", f.Path, err)
			}
			n, err := io.Copy(tw, io.LimitReader(r, f.Size+1))
			r.Close()
			if err != nil {
				return fmt.Errorf("copying %s: %w", f.Path, err)
			}
			if n != f.Size {
				return fmt.Errorf("%s: got %d bytes, want %d", f.Path, n, f.Size)
			}
		}
	}
	return tw.Close()
}

// formatFor: short ASCII paths (<=100 bytes) stay in USTAR so no PAX record
// is ever written; longer paths fall back to PAX, still without USTAR
// records of their own.
func formatFor(name string) tar.Format {
	if isUSTARName(name) {
		return tar.FormatUSTAR
	}
	return tar.FormatPAX
}

// isUSTARName reports whether name fits a plain USTAR path field.
func isUSTARName(name string) bool {
	if len(name) > 100 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] > 0x7e {
			return false
		}
	}
	return true
}

// collectDirs returns the sorted set of intermediate directories required by
// the given files.
func collectDirs(files []LayerFile) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		p := path.Clean("/" + strings.TrimPrefix(f.Path, "/"))
		for p != "/" && p != "." {
			p = path.Dir(p)
			if p == "/" || p == "." {
				break
			}
			if !seen[p] {
				seen[p] = true
				dirs = append(dirs, p)
			}
		}
	}
	sort.Strings(dirs)
	return dirs
}

// NewLayer returns a v1.Layer for the given files, compressed with codec.
// The layer content (tar then compression) is deterministic: two calls with
// identical inputs yield identical digests.
func NewLayer(files []LayerFile, codec compress.Codec, level int) (v1.Layer, error) {
	if codec == nil {
		return nil, fmt.Errorf("nil codec")
	}
	if _, _, def := codec.Levels(); level < 0 {
		level = def
	}
	var raw bytes.Buffer
	if err := BuildLayerTar(&raw, files); err != nil {
		return nil, err
	}
	compressed, err := compressBytes(codec, level, raw.Bytes())
	if err != nil {
		return nil, err
	}

	suffix := codec.MediaTypeSuffix()
	mt := types.OCIUncompressedLayer
	if suffix != "" && suffix != "none" {
		// base: application/vnd.oci.image.layer.v1.tar + suffix
		mt = types.MediaType(string(types.OCIUncompressedLayer) + "+" + suffix)
	}
	return &layer{
		content:   compressed,
		mediaType: mt,
		rawDigest: sha256Of(raw.Bytes()),
		codecID:   codec.ID(),
	}, nil
}

func compressBytes(codec compress.Codec, level int, src []byte) ([]byte, error) {
	var out bytes.Buffer
	w, err := codec.NewWriter(&out, level)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type layer struct {
	content   []byte
	mediaType types.MediaType
	rawDigest v1.Hash
	codecID   compress.ID
}

// Digest is computed lazily and cached.
func (l *layer) Digest() (v1.Hash, error) { return sha256Of(l.content), nil }

func (l *layer) DiffID() (v1.Hash, error) { return l.rawDigest, nil }

func (l *layer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.content)), nil
}

func (l *layer) Uncompressed() (io.ReadCloser, error) {
	codec, err := compress.ByID(l.codecID)
	if err != nil {
		return nil, err
	}
	return codec.NewReader(bytes.NewReader(l.content))
}

func (l *layer) Size() (int64, error) { return int64(len(l.content)), nil }

func (l *layer) MediaType() (types.MediaType, error) { return l.mediaType, nil }

func sha256Of(b []byte) v1.Hash {
	return v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", sha256.Sum256(b))}
}
