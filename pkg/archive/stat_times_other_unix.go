//go:build unix && !linux

package archive

import (
	"syscall"
	"time"
)

// Some Unix targets expose different Stat_t timestamp field names. Preserve
// portable metadata and leave unavailable birth/change times unset.
func statTimes(*syscall.Stat_t) (time.Time, time.Time) { return time.Time{}, time.Time{} }
