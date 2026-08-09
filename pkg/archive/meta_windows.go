//go:build windows

package archive

import (
	"fmt"
	"os"
)

func readMeta(path string, fi os.FileInfo, opts Options, e *Entry) error {
	e.ModTime = fi.ModTime()
	if opts.PreserveXattrs {
		return fmt.Errorf("xattrs are not supported on windows for %q", path)
	}
	return nil
}
