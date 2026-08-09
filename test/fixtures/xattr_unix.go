//go:build unix

package fixtures

import (
	"testing"

	"golang.org/x/sys/unix"
)

func setXattr(t *testing.T, path, name string, val []byte) {
	t.Helper()
	if err := unix.Setxattr(path, name, val, 0); err != nil {
		t.Fatalf("setxattr %s %s: %v", path, name, err)
	}
}
