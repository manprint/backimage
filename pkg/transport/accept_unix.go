//go:build unix

package transport

import "syscall"

// fatalAcceptErrno reports the accept errors that name a broken listening
// socket rather than a passing condition.
func fatalAcceptErrno(e syscall.Errno) bool {
	switch e {
	case syscall.EBADF, syscall.EINVAL, syscall.ENOTSOCK, syscall.EFAULT:
		return true
	}
	return false
}
