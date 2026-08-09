//go:build linux

package archive

import (
	"syscall"
	"time"
)

func statTimes(st *syscall.Stat_t) (time.Time, time.Time) {
	return time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec)),
		time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
}
