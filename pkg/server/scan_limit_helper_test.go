package server

import (
	"io"

	"github.com/manprint/backimage/pkg/index"
)

func scanWithLimit(r io.Reader, maxEntries int) ([]index.FileEntry, scanStats, error) {
	return scanArchive(r, maxEntries)
}
