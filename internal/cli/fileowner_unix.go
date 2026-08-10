//go:build !windows

package cli

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// fileOwner names the local user that owns the credential file: root when the
// logins were created under sudo, the plain user otherwise. That column is what
// explains why `sudo backimage login --list` shows a different set of accounts
// than the same command without sudo.
func fileOwner(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "-"
	}
	uid := strconv.FormatUint(uint64(stat.Uid), 10)
	if u, err := user.LookupId(uid); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid " + uid
}
