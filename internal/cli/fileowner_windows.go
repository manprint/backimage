//go:build windows

package cli

import (
	"os"
	"os/user"
)

// fileOwner reports the current user on Windows: the credential file has no
// portable uid, and there is no sudo split to explain.
func fileOwner(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "-"
	}
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "-"
	}
	return u.Username
}
