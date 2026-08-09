//go:build (unix && !linux) || windows

package archive

import "os"

func fileIdentity(os.FileInfo) (hardlinkKey, bool) { return hardlinkKey{}, false }
