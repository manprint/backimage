//go:build unix

package fixtures

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// CompareOptions relaxes checks when the platform cannot support them.
type CompareOptions struct {
	IgnoreOwner       bool
	IgnoreXattrs      bool
	IgnoreACLs        bool
	CompareAccessTime bool // atime is NOT compared by default; set to compare it (ctime is always ignored)
}

// Difference describes one mismatch in a human-readable form.
type Difference struct {
	Path  string
	Field string // "mode", "uid", "mtime", "xattr:user.foo", "content", "hardlink-group", …
	Want  string
	Got   string
}

// CompareTrees walks both trees and compares every path's type, mode,
// ownership, mtime, device numbers, content, xattrs and hardlink grouping.
// It is the single source of truth for "identical" in this project.
func CompareTrees(t *testing.T, want, got string, opts CompareOptions) []Difference {
	t.Helper()
	var diffs []Difference

	wantPaths := map[string]os.FileInfo{}
	walkPaths(t, want, wantPaths)
	gotPaths := map[string]os.FileInfo{}
	walkPaths(t, got, gotPaths)

	for rel, wfi := range wantPaths {
		gfi, ok := gotPaths[rel]
		if !ok {
			diffs = append(diffs, Difference{Path: rel, Field: "exists", Want: "present", Got: "missing"})
			continue
		}
		diffs = append(diffs, compareMeta(rel, want, got, wfi, gfi, opts)...)
	}
	for rel := range gotPaths {
		if _, ok := wantPaths[rel]; !ok {
			diffs = append(diffs, Difference{Path: rel, Field: "exists", Want: "absent", Got: "unexpected"})
		}
	}

	diffs = append(diffs, compareHardlinkGroups(t, want, got)...)
	return diffs
}

func walkPaths(t *testing.T, root string, out map[string]os.FileInfo) {
	t.Helper()
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Errorf("walk %s: %v", p, err)
			return nil
		}
		if p == root {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			t.Errorf("lstat %s: %v", p, err)
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Errorf("rel %s: %v", p, err)
			return nil
		}
		out[filepath.ToSlash(rel)] = fi
		return nil
	}); err != nil {
		t.Errorf("walk %s: %v", root, err)
	}
}

func compareMeta(rel, wantRoot, gotRoot string, wfi, gfi os.FileInfo, opts CompareOptions) []Difference {
	want := filepath.Join(wantRoot, filepath.FromSlash(rel))
	got := filepath.Join(gotRoot, filepath.FromSlash(rel))
	var diffs []Difference
	add := func(field, wantV, gotV string) {
		diffs = append(diffs, Difference{Path: rel, Field: field, Want: wantV, Got: gotV})
	}

	if wfi.Mode().Type() != gfi.Mode().Type() {
		add("type", wfi.Mode().Type().String(), gfi.Mode().Type().String())
		return diffs
	}

	if wfi.Mode().IsRegular() {
		sh1, sh2 := shaFileSt(want), shaFileSt(got)
		if sh1 != sh2 {
			add("content", sh1, sh2)
		}
	}

	if wfi.Mode()&fs.ModeSymlink != 0 {
		t1, err1 := os.Readlink(want)
		t2, err2 := os.Readlink(got)
		if err1 != nil || err2 != nil {
			add("readlink", fmt.Sprint(err1), fmt.Sprint(err2))
		} else if t1 != t2 {
			add("linktarget", t1, t2)
		}
	}

	ws, wok := wfi.Sys().(*syscall.Stat_t)
	gs, gok := gfi.Sys().(*syscall.Stat_t)
	if wok && gok {
		if wm, gm := ws.Mode&0o7777, gs.Mode&0o7777; wm != gm {
			add("mode", fmt.Sprintf("%o", wm), fmt.Sprintf("%o", gm))
		}
		if !opts.IgnoreOwner {
			if ws.Uid != gs.Uid {
				add("uid", fmt.Sprint(ws.Uid), fmt.Sprint(gs.Uid))
			}
			if ws.Gid != gs.Gid {
				add("gid", fmt.Sprint(ws.Gid), fmt.Sprint(gs.Gid))
			}
		}
		if opts.CompareAccessTime && (ws.Atim.Sec != gs.Atim.Sec || ws.Atim.Nsec != gs.Atim.Nsec) {
			add("atime", fmt.Sprint(ws.Atim), fmt.Sprint(gs.Atim))
		}
		if wfi.ModTime().UnixNano() != gfi.ModTime().UnixNano() {
			add("mtime", wfi.ModTime().UTC().String(), gfi.ModTime().UTC().String())
		}
		if gfi.Mode()&(fs.ModeDevice|fs.ModeCharDevice) != 0 && ws.Rdev != gs.Rdev {
			add("dev", fmt.Sprint(ws.Rdev), fmt.Sprint(gs.Rdev))
		}
	}

	if !opts.IgnoreXattrs {
		wx := readXattrsAll(want)
		gx := readXattrsAll(got)
		if opts.IgnoreACLs {
			stripACLs(wx)
			stripACLs(gx)
		}
		for k, v := range wx {
			gv, ok := gx[k]
			if !ok {
				add("xattr:"+k, fmt.Sprintf("%q", v), "<missing>")
				continue
			}
			if string(v) != string(gv) {
				add("xattr:"+k, fmt.Sprintf("%q", v), fmt.Sprintf("%q", gv))
			}
		}
		for k := range gx {
			if _, ok := wx[k]; !ok {
				add("xattr:"+k, "<absent>", "present")
			}
		}
	}
	return diffs
}

func stripACLs(m map[string][]byte) {
	for k := range m {
		if strings.HasPrefix(k, "system.posix_acl") || strings.HasPrefix(k, "security.selinux") {
			delete(m, k)
		}
	}
}

func readXattrsAll(path string) map[string][]byte {
	out := map[string][]byte{}
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		return out
	}
	if size == 0 {
		return out
	}
	buf := make([]byte, size)
	n, err := unix.Llistxattr(path, buf)
	if err != nil {
		return out
	}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:n]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		vsz, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			continue
		}
		val := make([]byte, vsz)
		vn, err := unix.Lgetxattr(path, name, val)
		if err == nil {
			out[name] = val[:vn]
		}
	}
	return out
}

func shaFileSt(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return "ERR:unreadable"
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "ERR:unreadable"
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func compareHardlinkGroups(t *testing.T, want, got string) []Difference {
	wGroups := map[string]string{} // rel path -> canonical group key
	if err := partitionGroups(wGroups, want); err != nil {
		t.Errorf("group walk %s: %v", want, err)
		return nil
	}
	gGroups := map[string]string{}
	if err := partitionGroups(gGroups, got); err != nil {
		t.Errorf("group walk %s: %v", got, err)
		return nil
	}
	var diffs []Difference
	for rel, wKey := range wGroups {
		gKey, ok := gGroups[rel]
		if !ok {
			diffs = append(diffs, Difference{Path: rel, Field: "hardlink-group", Want: wKey, Got: "missing"})
			continue
		}
		if wKey != gKey {
			diffs = append(diffs, Difference{Path: rel, Field: "hardlink-group", Want: wKey, Got: gKey})
		}
	}
	for rel := range gGroups {
		if _, ok := wGroups[rel]; !ok {
			diffs = append(diffs, Difference{Path: rel, Field: "hardlink-group", Want: "absent", Got: "present"})
		}
	}
	return diffs
}

// partitionGroups maps every regular path with nlink>1 to a canonical key of
// the sorted paths sharing its inode. Keys are path-based, not inode-based, so
// the copy tree (new inodes) still lands on identical keys for identical
// topologies.
func partitionGroups(out map[string]string, root string) error {
	groups := map[[2]uint64][]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err // raced removal aborts grouping; caller reports and degrades
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Nlink <= 1 {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		key := [2]uint64{uint64(st.Dev), st.Ino}
		groups[key] = append(groups[key], filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	for _, group := range groups {
		sort.Strings(group)
		canon := strings.Join(group, ",")
		for _, rel := range group {
			out[rel] = canon
		}
	}
	return nil
}
