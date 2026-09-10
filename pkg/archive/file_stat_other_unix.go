//go:build unix && !linux

package archive

import (
	"os"
	"syscall"
)

func unixFileDevice(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(uint32(st.Dev)), true
}
