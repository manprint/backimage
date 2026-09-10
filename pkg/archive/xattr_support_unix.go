//go:build unix

package archive

// xattrsSupported reports whether the platform has extended attributes at
// all. A filesystem that answers ENOTSUP is a separate, per-path matter.
const xattrsSupported = true
