package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type tarReader struct {
	tr *tar.Reader
}

func newReader(r io.Reader) Reader {
	return &tarReader{tr: tar.NewReader(r)}
}

// Next reads the next entry. Returns io.EOF at the end.
func (r *tarReader) Next() (*Entry, io.Reader, error) {
	hdr, err := r.tr.Next()
	if err != nil {
		return nil, nil, err
	}
	e := &Entry{
		Path:       CleanPath(hdr.Name),
		Mode:       hdr.FileInfo().Mode(),
		UID:        hdr.Uid,
		GID:        hdr.Gid,
		Uname:      hdr.Uname,
		Gname:      hdr.Gname,
		ModTime:    hdr.ModTime,
		AccessTime: hdr.AccessTime, // stdlib consumes PAX "atime"
		ChangeTime: hdr.ChangeTime, // stdlib consumes PAX "ctime"
		DevMajor:   hdr.Devmajor,
		DevMinor:   hdr.Devminor,
		LinkTarget: hdr.Linkname,
	}
	if e.Type == TypeRegular {
		e.Size = hdr.Size
	}
	switch hdr.Typeflag {
	case tar.TypeReg:
		e.Type = TypeRegular
	case tar.TypeDir:
		e.Type = TypeDir
	case tar.TypeSymlink:
		e.Type = TypeSymlink
	case tar.TypeLink:
		e.Type = TypeHardlink
	case tar.TypeChar:
		e.Type = TypeCharDevice
	case tar.TypeBlock:
		e.Type = TypeBlockDevice
	case tar.TypeFifo:
		e.Type = TypeFifo
	default:
		return nil, nil, fmt.Errorf("tar entry %q: unsupported typeflag %q", hdr.Name, hdr.Typeflag)
	}
	if hdr.PAXRecords != nil {
		for k, v := range hdr.PAXRecords {
			if rest, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
				if e.Xattrs == nil {
					e.Xattrs = map[string][]byte{}
				}
				// The reader escapes UTF-8-encoded sequences; PAX values are
				// length-prefixed, so binary values round-trip byte-for-byte.
				e.Xattrs[rest] = []byte(v)
			}
		}
	}
	if err := e.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid entry %q: %w", hdr.Name, err)
	}
	return e, r.tr, nil
}

func parsePAXTime(v string) (time.Time, error) {
	var sec, nsec int64
	_, err := fmt.Sscanf(v, "%d.%d", &sec, &nsec)
	if err != nil {
		_, err2 := fmt.Sscanf(v, "%d", &sec)
		if err2 != nil {
			return time.Time{}, err
		}
	}
	return time.Unix(sec, nsec), nil
}

var _ = os.FileMode(0)
