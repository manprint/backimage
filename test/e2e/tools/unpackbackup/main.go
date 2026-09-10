// Command unpackbackup writes the /backup tree of a backimage image out of an
// OCI layout, so a backup produced by any build can be frozen as a directory
// of files instead of a stack of opaque blobs.
//
// It exists for the format fixtures: `scripts/make-format-fixtures.sh` builds
// a backup with a given release of the tool and unpacks it here, and the
// fixtures then feed recovery.OpenLocal directly. Keeping them as a tree
// rather than as a layout means the JSON metadata stays readable in a diff,
// which is the point of freezing them.
package main

import (
	"archive/tar"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
)

// maxEntryBytes bounds a single extracted file. The fixtures are a few
// kilobytes; anything past this is a corrupted layer, not a fixture.
const maxEntryBytes = 64 << 20

func main() {
	src := flag.String("layout", "", "OCI layout directory holding one backimage image")
	dst := flag.String("out", "", "directory to write the backup tree into")
	flag.Parse()
	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: unpackbackup -layout DIR -out DIR")
		os.Exit(2)
	}
	if err := run(*src, *dst); err != nil {
		fmt.Fprintln(os.Stderr, "unpackbackup:", err)
		os.Exit(1)
	}
}

func run(src, dst string) error {
	lp, err := layout.FromPath(src)
	if err != nil {
		return err
	}
	idx, err := lp.ImageIndex()
	if err != nil {
		return err
	}
	img, err := firstImage(idx)
	if err != nil {
		return err
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	for _, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			return err
		}
		err = extract(rc, dst)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// firstImage walks the layout down to the first real image manifest. A
// backimage layout holds one index per platform; every platform carries the
// same backup, so the first one is enough.
func firstImage(idx v1.ImageIndex) (v1.Image, error) {
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, d := range im.Manifests {
		if d.MediaType.IsIndex() {
			child, err := idx.ImageIndex(d.Digest)
			if err != nil {
				return nil, err
			}
			return firstImage(child)
		}
		if d.MediaType.IsImage() {
			return idx.Image(d.Digest)
		}
	}
	return nil, fmt.Errorf("no image in the layout")
}

// extract writes the entries of one layer under dst. Only regular files and
// directories appear in a backimage layer; anything else is a sign the layout
// is not what this tool is for, so it is an error rather than a silent skip.
func extract(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Layer entries are absolute (/backup/..., /backimage). Only the
		// backup tree is wanted: the extractor binary is megabytes and says
		// nothing about the on-disk format.
		clean := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "/")
		if clean != "backup" && !strings.HasPrefix(clean, "backup/") {
			continue
		}
		name := filepath.Clean(filepath.FromSlash(clean))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe entry %q", hdr.Name)
		}
		target := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			// A fixture layer is ours and tiny; the cap is here so a
			// corrupted one cannot fill the disk instead of failing.
			if _, err := io.CopyN(f, tr, maxEntryBytes); err != nil && !errors.Is(err, io.EOF) {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected entry type %q in %q", string(hdr.Typeflag), hdr.Name)
		}
	}
}
