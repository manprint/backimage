//go:build windows

package archive

import "os"

// Windows has no inode: NTFS hard links exist but Go exposes no identity for
// them, so every path is archived as its own regular file.
func fileIdentity(os.FileInfo) (hardlinkKey, bool) { return hardlinkKey{}, false }
