//go:build unix && !linux

package archive

import (
	"syscall"
	"time"
)

// Darwin and the BSDs name the timestamp fields *timespec instead of *tim.
// Returning zero times here used to drop the access and change time of every
// entry archived outside Linux.
func statTimes(st *syscall.Stat_t) (time.Time, time.Time) {
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec),
		time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}
