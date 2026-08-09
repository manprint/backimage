//go:build linux

package archive

import (
	"os"
	"syscall"
)

func fileIdentity(fi os.FileInfo) (hardlinkKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return hardlinkKey{}, false
	}
	return hardlinkKey{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, true
}
