//go:build windows

package archive

import "os"

// Windows carries no POSIX extended attributes and no uid/gid, so a request
// to preserve them has nothing to fail on: the writer reports the gap once,
// from xattrsSupported, and every entry keeps the metadata the platform does
// have. Returning an error here dropped every entry of a Windows backup.
func readMeta(_ string, fi os.FileInfo, _ Options, e *Entry) error {
	e.ModTime = fi.ModTime()
	return nil
}
