//go:build unix && !linux

package archive

// POSIX UTIME_OMIT. x/sys/unix does not expose this constant on every Unix
// target supported by the project.
const utimeOmit = 1<<30 - 2
