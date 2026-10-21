//go:build darwin

package archive

// UTIME_OMIT from <sys/stat.h>; x/sys/unix does not export it for darwin.
// The Linux value, used here before, is an invalid tv_nsec to utimensat(2),
// so every timestamp a restore applied on macOS failed with EINVAL.
const utimeOmit = -2
