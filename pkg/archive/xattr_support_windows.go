//go:build windows

package archive

// Windows has no POSIX extended attributes: asking to preserve them is not an
// error, there is simply nothing to carry.
const xattrsSupported = false
