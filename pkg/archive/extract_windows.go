//go:build windows

package archive

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Windows has no POSIX ownership/xattr primitives. Keep the portable archive
// semantics (files, directories and symlinks) so cross-built binaries remain
// usable while preserving the explicit platform limitation.
type windowsExtractor struct{ opts ExtractOptions }

func extractorFor(opts ExtractOptions) Extractor { return &windowsExtractor{opts: opts} }

func (x *windowsExtractor) Extract(ctx context.Context, r io.Reader, dest string) (Stats, error) {
	var stats Stats
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			return stats, nil
		}
		if err != nil {
			return stats, err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return stats, fmt.Errorf("archive path traversal: %q", h.Name)
		}
		path := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(h.Mode)); err != nil {
				return stats, err
			}
			stats.Dirs++
		case tar.TypeSymlink:
			if !x.opts.Overwrite {
				if _, err := os.Lstat(path); err == nil {
					return stats, fmt.Errorf("path exists: %s", h.Name)
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return stats, err
			}
			if err := os.Symlink(h.Linkname, path); err != nil {
				return stats, err
			}
			stats.Symlinks++
		default:
			if !x.opts.Overwrite {
				if _, err := os.Lstat(path); err == nil {
					return stats, fmt.Errorf("path exists: %s", h.Name)
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return stats, err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode))
			if err != nil {
				return stats, err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return stats, copyErr
			}
			if closeErr != nil {
				return stats, closeErr
			}
			stats.Files++
		}
	}
}
