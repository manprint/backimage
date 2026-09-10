//go:build unix && !linux

package archive

import (
	"os"
	"syscall"
)

// The field widths of Stat_t differ from Linux (Dev is signed, Nlink is 16
// bits), which is the only reason this is not the Linux file: without it a
// darwin backup archived every hard link as a full copy of the payload.
func fileIdentity(fi os.FileInfo) (hardlinkKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return hardlinkKey{}, false
	}
	return hardlinkKey{Dev: uint64(uint32(st.Dev)), Ino: uint64(st.Ino)}, true
}
