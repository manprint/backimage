//go:build windows

package transport

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// fatalAcceptErrno reports the accept errors that name a broken listening
// socket rather than a passing condition. Winsock reports its own codes;
// syscall's portable names are accepted too, so the classification reads the
// same as on unix.
func fatalAcceptErrno(e syscall.Errno) bool {
	switch e {
	case windows.WSAEBADF, windows.WSAEINVAL, windows.WSAENOTSOCK, windows.WSAEFAULT,
		syscall.EBADF, syscall.EINVAL, syscall.ENOTSOCK, syscall.EFAULT:
		return true
	}
	return false
}
