//go:build windows

package archive

import "os"

func fileDevice(os.FileInfo) (uint64, bool) { return 0, false }
