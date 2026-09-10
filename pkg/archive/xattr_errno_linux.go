//go:build linux

package archive

import (
	"errors"

	"golang.org/x/sys/unix"
)

// isMissingXattr reports the errno the kernel returns when the named
// attribute does not exist on the file. Linux answers ENODATA and the BSDs
// answer ENOATTR; x/sys/unix declares only the one its own platform uses, so
// the two cannot be tested in a single expression.
func isMissingXattr(err error) bool { return errors.Is(err, unix.ENODATA) }
