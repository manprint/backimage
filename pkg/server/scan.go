package server

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/manprint/backimage/pkg/archive"
	"github.com/manprint/backimage/pkg/index"
)

// scanStats mirrors archive.Stats for a stream the server did not produce.
type scanStats struct {
	Files     int64
	Dirs      int64
	Symlinks  int64
	Hardlinks int64
	Devices   int64
	BytesRaw  int64
}

// scanArchive rebuilds the per-file index from the raw tar stream. The client
// no longer sends entries: the server sees the same bytes and derives offsets
// and payload digests while the data flows to the chunker.
//
// TarOffset uses the same convention as archive.Writer: the logical start of
// the entry header, rounded up to the next 512-byte boundary.
func scanArchive(r io.Reader) ([]index.FileEntry, scanStats, error) {
	var stats scanStats
	counter := &countingReader{r: r}
	tr := tar.NewReader(counter)
	entries := make([]index.FileEntry, 0, 256)
	for {
		offset := (counter.n + 511) &^ int64(511)
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries, stats, nil
		}
		if err != nil {
			return entries, stats, fmt.Errorf("read archive stream: %w", err)
		}
		entry := index.FileEntry{
			Path:       archive.CleanPath(hdr.Name),
			Type:       entryType(hdr.Typeflag),
			Mode:       index.FormatMode(tarMode(hdr)),
			UID:        hdr.Uid,
			GID:        hdr.Gid,
			UName:      hdr.Uname,
			GName:      hdr.Gname,
			MTime:      hdr.ModTime,
			LinkTarget: hdr.Linkname,
			TarOffset:  offset,
		}
		if entry.Type == "" {
			return entries, stats, fmt.Errorf("tar entry %q: unsupported typeflag %q", hdr.Name, hdr.Typeflag)
		}
		if hdr.Typeflag == tar.TypeReg {
			entry.Size = hdr.Size
			sum := sha256.New()
			// Bounded by the declared entry size: a lying header cannot make
			// the scanner read more than the tar says it holds.
			n, copyErr := io.Copy(sum, io.LimitReader(tr, hdr.Size))
			if copyErr != nil {
				return entries, stats, fmt.Errorf("read %q from stream: %w", hdr.Name, copyErr)
			}
			entry.SHA256 = hex.EncodeToString(sum.Sum(nil))
			stats.BytesRaw += n
		}
		countEntry(&stats, hdr.Typeflag)
		entries = append(entries, entry)
	}
}

func countEntry(stats *scanStats, typeflag byte) {
	switch typeflag {
	case tar.TypeReg:
		stats.Files++
	case tar.TypeDir:
		stats.Dirs++
	case tar.TypeSymlink:
		stats.Symlinks++
	case tar.TypeLink:
		stats.Hardlinks++
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		stats.Devices++
	}
}

func entryType(typeflag byte) string {
	switch typeflag {
	case tar.TypeReg:
		return index.TypeRegular
	case tar.TypeDir:
		return index.TypeDir
	case tar.TypeSymlink:
		return index.TypeSymlink
	case tar.TypeLink:
		return index.TypeHardlink
	case tar.TypeChar:
		return index.TypeChar
	case tar.TypeBlock:
		return index.TypeBlock
	case tar.TypeFifo:
		return index.TypeFifo
	}
	return ""
}

// tarMode reproduces the permission encoding used by the local pipeline.
func tarMode(hdr *tar.Header) uint32 {
	mode := hdr.FileInfo().Mode()
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

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
