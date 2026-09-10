//go:build unix && !linux

package archive

import (
	"errors"

	"golang.org/x/sys/unix"
)

func isMissingXattr(err error) bool { return errors.Is(err, unix.ENOATTR) }
