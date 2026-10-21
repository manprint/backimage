//go:build unix || windows

package archive

import (
	"fmt"
	"io"
)

// drainTrailing reads whatever follows the end-of-archive marker. Extract is
// usually fed by a producer writing into a pipe — the restore of a backup,
// the self-extracting image — and a reader that stops at the marker leaves
// that producer blocked forever on the bytes after it, with the restore
// waiting on the producer. Reading them also lets the producer verify the
// chunks that carry them. backimage never writes anything after the marker,
// so the caller warns when there was something.
func drainTrailing(r io.Reader) (int64, error) {
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return n, fmt.Errorf("reading past the end of the archive: %w", err)
	}
	return n, nil
}

func trailingWarning(n int64) string {
	return fmt.Sprintf("%d byte dopo la fine dell'archivio sono stati ignorati", n)
}
