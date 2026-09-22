//go:build linux

package archive

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestBackupLeavesSourceAccessTimesAlone: the backup must not modify the
// source in any way, and an access time is part of the source. That holds for
// all three walks a backup makes over it: preflight, estimate and archive. The atimes are
// set older than the mtimes, which is exactly when relatime — the default
// mount option — updates them on a read without O_NOATIME.
func TestBackupLeavesSourceAccessTimesAlone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "file")
	if err := os.WriteFile(file, []byte("content read by the backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	mtime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{file, sub, root} {
		if err := os.Chtimes(p, old, mtime); err != nil {
			t.Fatal(err)
		}
	}
	archivePaths(t, root, Options{Strict: true, PreserveXattrs: true})
	if _, err := PreflightBackup(context.Background(), []string{root}); err != nil {
		t.Fatal(err)
	}
	if _, err := Estimate(context.Background(), []string{root}, Options{Strict: true}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, sub, root} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		atime, _ := statTimes(fi.Sys().(*syscall.Stat_t))
		if !atime.Equal(old) {
			t.Errorf("%s: atime changed by the backup: %v, was %v", p, atime, old)
		}
	}
}
