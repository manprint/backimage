//go:build linux

package archive

import "syscall"

// noAtimeFlag keeps reading the source from touching its access times: the
// backup must leave the tree exactly as it found it.
const noAtimeFlag = syscall.O_NOATIME
