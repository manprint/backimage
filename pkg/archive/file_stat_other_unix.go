//go:build unix && !linux

package archive

import "os"

func unixFileDevice(os.FileInfo) (uint64, bool) { return 0, false }
