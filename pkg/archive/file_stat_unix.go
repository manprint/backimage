//go:build unix

package archive

import "os"

func fileDevice(fi os.FileInfo) (uint64, bool) {
	return unixFileDevice(fi)
}
