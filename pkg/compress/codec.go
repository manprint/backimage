// Package compress exposes every compression backend behind a single
// registered interface (decision D14).
//
// Wire identifiers are stable: Store=0, Gzip=1, Zstd=2, Xz=3, Lz4=4. IDs are
// written into the blob envelope and must never be removed or renumbered.
package compress

import (
	"fmt"
	"io"
)

// ID identifies a compression algorithm on the wire and in the blob envelope.
type ID uint8

const (
	Store ID = 0
	Gzip  ID = 1
	Zstd  ID = 2
	Xz    ID = 3
	Lz4   ID = 4
)

// Codec compresses and decompresses a byte stream.
type Codec interface {
	// ID returns the wire identifier written into the blob envelope.
	ID() ID
	// Name returns the user-facing name, e.g. "zstd".
	Name() string
	// MediaTypeSuffix returns the OCI layer media type suffix ("gzip", "zstd")
	// or "" when the algorithm has no standard OCI media type. "none" is the
	// special value for Store: an uncompressed tar layer still has a valid
	// OCI media type.
	MediaTypeSuffix() string
	// Levels returns the valid level range, inclusive.
	Levels() (min, max, def int)
	// NewWriter wraps w. Closing the returned writer flushes but does not close w.
	NewWriter(w io.Writer, level int) (io.WriteCloser, error)
	// NewReader wraps r.
	NewReader(r io.Reader) (io.ReadCloser, error)
}

// UsageErrorf builds an error that internal/cli maps to exit code 2
// (KindUsage) through the UsageError() marker interface.
func UsageErrorf(format string, args ...interface{}) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

type usageError struct{ msg string }

func (e *usageError) Error() string    { return e.msg }
func (e *usageError) UsageError() bool { return true }
