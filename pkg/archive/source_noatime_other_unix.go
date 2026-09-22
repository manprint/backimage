//go:build unix && !linux

package archive

// noAtimeFlag is zero where the platform has no O_NOATIME: reads update the
// access time as the mount options say.
const noAtimeFlag = 0
