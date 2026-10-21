//go:build unix && !linux && !darwin && !netbsd

package archive

import "golang.org/x/sys/unix"

// UTIME_OMIT differs between kernels: -2 on FreeBSD, -1 on OpenBSD. Using the
// Linux value here made every timestamp a restore applied fail with EINVAL.
const utimeOmit = unix.UTIME_OMIT
