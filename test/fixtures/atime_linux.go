//go:build linux

package fixtures

import "syscall"

// atimeOf returns the access time of a stat result in seconds and nanoseconds.
// The field name differs across Unix targets, which is the whole reason this
// lives in its own file.
func atimeOf(st *syscall.Stat_t) (int64, int64) {
	return int64(st.Atim.Sec), int64(st.Atim.Nsec)
}
