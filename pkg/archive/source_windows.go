//go:build windows

package archive

import "os"

func openSource(dir *os.Root, name, path string) (*os.File, error) {
	if dir == nil {
		return os.Open(path)
	}
	return dir.Open(name)
}

func settleSource(*os.File) error { return nil }

// Windows exposes no inode through os.FileInfo (see fileIdentity), so only
// the type of the entry can be compared.
func sameInode(_, _ os.FileInfo) bool { return true }
