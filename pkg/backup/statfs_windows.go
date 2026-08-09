//go:build windows

package backup

// Windows has no portable syscall in x/sys/unix. The pipeline still performs
// normal file errors while the preflight free-space check is unavailable.
func platformFreeSpace(string) (int64, error) { return int64(^uint64(0) >> 1), nil }
