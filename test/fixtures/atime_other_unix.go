//go:build unix && !linux

package fixtures

import "syscall"

func atimeOf(st *syscall.Stat_t) (int64, int64) {
	return int64(st.Atimespec.Sec), int64(st.Atimespec.Nsec)
}
