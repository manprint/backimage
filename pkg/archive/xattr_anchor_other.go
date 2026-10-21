//go:build unix && !linux

package archive

// xattrPath returns the path the attributes of src are read through. These
// platforms have no by-path xattr call relative to a directory descriptor, so
// it is the full path; the caller's identity checks around the read are what
// keeps a swapped parent from going unnoticed.
func xattrPath(src source) (path string, release func(), err error) {
	return src.path, func() {}, nil
}
