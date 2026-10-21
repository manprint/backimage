//go:build netbsd

package archive

// UTIME_OMIT from NetBSD's <sys/stat.h>, the same value as on Linux; x/sys/unix
// does not export it for netbsd.
const utimeOmit = 1<<30 - 2
