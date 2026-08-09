//go:build unix

package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type extractor struct {
	opts ExtractOptions
}

func extractorFor(opts ExtractOptions) Extractor {
	return &extractor{opts: opts}
}

// Metadata application order per entry (mandatory, see docs/FIDELITY.md):
//
//  1. create the object
//  2. write the content (regular files only)
//  3. lchown(uid, gid)          <- before chmod: chown clears setuid/setgid
//  4. chmod(mode)               <- not for symlinks (no lchmod on Linux)
//  5. setxattr(...)             <- after chown: security.capability cleared by chown
//  6. utimes(atime, mtime)      <- last per-entry metadata step
//
// After everything: re-apply mode and timestamps to all directories, deepest
// first (writing into a directory changes its mtime; a 0500 directory is not
// writable until populated).
func (x *extractor) Extract(ctx context.Context, r io.Reader, dest string) (Stats, error) {
	var stats Stats
	tr := tar.NewReader(r)
	dest = filepath.Clean(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return stats, fmt.Errorf("mkdir dest %q: %w", dest, err)
	}
	type dirFix struct {
		path string
		hdr  *tar.Header
		at   time.Time
		mt   time.Time
	}
	var dirFixes []dirFix

	for {
		if err := ctx.Err(); err != nil {
			return stats, fmt.Errorf("extracting to %q: %w", dest, err)
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("tar read: %w", err)
		}
		name := CleanPath(hdr.Name)
		if !x.matches(name) {
			continue
		}
		name, ok := stripComponents(name, x.opts.StripComponents)
		if !ok {
			continue
		}
		hdr.Name = name
		if hdr.Typeflag == tar.TypeLink {
			link, keep := stripComponents(CleanPath(hdr.Linkname), x.opts.StripComponents)
			if !keep {
				continue
			}
			hdr.Linkname = link
		}
		target, err := safeJoin(dest, name)
		if err != nil {
			return stats, x.maybe(err)
		}
		if err := x.createOne(ctx, dest, target, hdr, tr, &stats); err != nil {
			if isPerm(err) {
				if derr := x.maybe(err); derr == nil {
					stats.Skipped++
					continue
				}
			}
			return stats, err
		}
		if hdr.Typeflag == tar.TypeDir {
			dirFixes = append(dirFixes, dirFix{
				path: target,
				hdr:  hdr,
				at:   hdr.AccessTime,
				mt:   hdr.ModTime,
			})
		}
	}
	// Directories: deepest first.
	sort.Slice(dirFixes, func(i, j int) bool {
		return len(dirFixes[i].path) > len(dirFixes[j].path)
	})
	for _, d := range dirFixes {
		if err := os.Chmod(d.path, headerMode(d.hdr)); err != nil {
			return stats, x.maybe(fmt.Errorf("chmod dir %q: %w", d.path, err))
		}
		at := unix.Timespec{Nsec: unix.UTIME_OMIT}
		if !d.at.IsZero() {
			at = unix.NsecToTimespec(d.at.UnixNano())
		}
		if !d.mt.IsZero() {
			ts := []unix.Timespec{
				at,
				unix.NsecToTimespec(d.mt.UnixNano()),
			}
			if err := unix.UtimesNanoAt(unix.AT_FDCWD, d.path, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return stats, x.maybe(fmt.Errorf("utimes dir %q: %w", d.path, err))
			}
		}
	}
	return stats, nil
}

func stripComponents(name string, count int) (string, bool) {
	if count <= 0 {
		return name, name != ""
	}
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) <= count {
		return "", false
	}
	return strings.Join(parts[count:], "/"), true
}

func (x *extractor) matches(name string) bool {
	if len(x.opts.Includes) > 0 {
		ok := false
		for _, pat := range x.opts.Includes {
			if m, err := filepath.Match(pat, name); err == nil && m {
				ok = true
				break
			}
			if strings.HasSuffix(pat, "/") && strings.HasPrefix(name, strings.TrimSuffix(pat, "/")+"/") {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, pat := range x.opts.Excludes {
		if m, err := filepath.Match(pat, name); err == nil && m {
			return false
		}
		if strings.HasSuffix(pat, "/") && strings.HasPrefix(name, strings.TrimSuffix(pat, "/")+"/") {
			return false
		}
	}
	return true
}

// safeJoin resolves target under dest, refusing traversal and symlink swaps.
func safeJoin(dest, name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || name == ".." ||
		strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	target := filepath.Join(dest, filepath.FromSlash(name))
	// Check the resolved path stays under dest.
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path %q escapes destination", name)
	}
	// Symlink swap defense: walk existing components with O_NOFOLLOW.
	cur := dest
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := 0; i < len(parts); i++ {
		cur = filepath.Join(cur, parts[i])
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", fmt.Errorf("lstat %q: %w", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 && i < len(parts)-1 {
			// Existing symlink used as an intermediate component: resolve and
			// refuse if it points outside dest.
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", fmt.Errorf("resolve symlink %q: %w", cur, err)
			}
			if !under(dest, real) {
				return "", fmt.Errorf("symlink %q escapes destination", cur)
			}
		}
	}
	return target, nil
}

func under(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, "../")
}

func (x *extractor) createOne(ctx context.Context, dest, target string, hdr *tar.Header, tr *tar.Reader, stats *Stats) error {
	// Overwrite handling.
	if _, err := os.Lstat(target); err == nil {
		if !x.opts.Overwrite {
			return fmt.Errorf("%q already exists (use --overwrite)", target)
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove existing %q: %w", target, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %q: %w", target, err)
	}

	// Intermediate dirs may be missing in manipulated archives: create them
	// 0700, the final chmod pass re-fixes them.
	if hdr.Typeflag != tar.TypeDir {
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			if isPerm(err) {
				return permError("mkdir parent "+target, err)
			}
			return fmt.Errorf("mkdir parent %q: %w", target, err)
		}
	}

	mode := fs.FileMode(uint32(hdr.Mode)) & fs.ModePerm // #nosec G115 -- mode is 12 bits
	switch hdr.Typeflag {
	case tar.TypeDir:
		// Create intermediate dirs (archives may be manipulated).
		if err := os.MkdirAll(target, 0o700); err != nil {
			if isPerm(err) {
				return permError("mkdir "+target, err)
			}
			return fmt.Errorf("mkdir %q: %w", target, err)
		}
		stats.Dirs++
	case tar.TypeReg:
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, mode)
		if err != nil {
			return fmt.Errorf("create %q: %w", target, err)
		}
		if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
			f.Close()
			return fmt.Errorf("write %q: %w", target, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %q: %w", target, err)
		}
		stats.Files++
		stats.BytesRaw += hdr.Size
	case tar.TypeSymlink:
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			if isPerm(err) {
				return permError("symlink "+hdr.Linkname, err)
			}
			return fmt.Errorf("symlink %q: %w", target, err)
		}
		stats.Symlinks++
	case tar.TypeLink:
		first := filepath.Join(dest, filepath.FromSlash(hdr.Linkname))
		if err := os.Link(first, target); err != nil {
			return fmt.Errorf("hardlink %q -> %q: %w", target, first, err)
		}
		stats.Hardlinks++
	case tar.TypeChar, tar.TypeBlock:
		typ := uint32(unix.S_IFCHR)
		if hdr.Typeflag == tar.TypeBlock {
			typ = unix.S_IFBLK
		}
		dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))          // #nosec G115 -- devmajor/minor are 32-bit in kernel
		if err := unix.Mknod(target, typ|uint32(mode), int(dev)); err != nil { // #nosec G115 -- dev is a kernel rdev, fit in int
			if isPerm(err) {
				return permError("mknod", err)
			}
			return fmt.Errorf("mknod %q: %w", target, err)
		}
		stats.Devices++
	case tar.TypeFifo:
		if err := unix.Mkfifo(target, uint32(mode)); err != nil {
			if isPerm(err) {
				return permError("mkfifo", err)
			}
			return fmt.Errorf("mkfifo %q: %w", target, err)
		}
		stats.Fifos++
	default:
		return fmt.Errorf("%q: unsupported typeflag %q", hdr.Name, hdr.Typeflag)
	}

	// Order (mandatory, see docs/FIDELITY.md):
	//  3. lchown(uid, gid)  <- after creation, before chmod (chown clears setuid/setgid)
	//  4. chmod(mode)       <- not for symlinks (no lchmod on Linux)
	//  5. setxattr(...)     <- after chown (security.capability cleared by chown)
	//  6. utimes(atime, mtime) <-- last
	if x.opts.PreserveOwner {
		if err := unix.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
			if isPerm(err) {
				return permError("chown", err)
			}
			if err := x.maybe(fmt.Errorf("lchown %q: %w", target, err)); err != nil {
				return err
			}
		}
	}

	// chmod (not for symlinks). Directories are chmod'd in the final pass
	// (deepest-first), never here: a 0500 dir must be writable while its
	// children are being created.
	if hdr.Typeflag != tar.TypeSymlink && hdr.Typeflag != tar.TypeDir {
		if err := os.Chmod(target, headerMode(hdr)); err != nil {
			return x.maybe(fmt.Errorf("chmod %q: %w", target, err))
		}
	}
	// xattrs after chown (capabilities are cleared by chown).
	if x.opts.PreserveXattrs && hdr.PAXRecords != nil {
		for k, v := range hdr.PAXRecords {
			rest, ok := strings.CutPrefix(k, "SCHILY.xattr.")
			if !ok {
				continue
			}
			if err := unix.Lsetxattr(target, rest, []byte(v), 0); err != nil {
				if isPerm(err) {
					return permError("setxattr "+rest, err)
				}
				return x.maybe(fmt.Errorf("setxattr %q %s: %w", target, rest, err))
			}
		}
	}
	// timestamps last (lutimes semantics: symlink-safe). atime is omitted
	// when the archive carries no value for it (UTIME_OMIT keeps the
	// extraction-time atime instead of clamping it to the epoch).
	at := unix.Timespec{Nsec: unix.UTIME_OMIT}
	if !hdr.AccessTime.IsZero() {
		at = unix.NsecToTimespec(hdr.AccessTime.UnixNano())
	}
	if !hdr.ModTime.IsZero() {
		ts := []unix.Timespec{
			at,
			unix.NsecToTimespec(hdr.ModTime.UnixNano()),
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, target, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return x.maybe(fmt.Errorf("utimes %q: %w", target, err))
		}
	}
	return nil
}

// headerMode reconstructs an os.FileMode from a tar header, including the
// setuid/setgid/sticky bits (Perm() alone would drop them).
func headerMode(hdr *tar.Header) fs.FileMode {
	m := fs.FileMode(hdr.Mode & 0o7777) // #nosec G115 -- masked to 12 bits
	if hdr.Mode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if hdr.Mode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if hdr.Mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

func (x *extractor) maybe(err error) error {
	if x.opts.Strict {
		return err
	}
	return nil // degraded mode: skip silently (caller counts)
}

func isPerm(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

func permError(op string, err error) error {
	return &PermissionHintError{Op: op, Err: err}
}

// PermissionHintError carries a user-facing remediation for privilege failures.
type PermissionHintError struct {
	Op  string
	Err error
}

func (e *PermissionHintError) Error() string {
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *PermissionHintError) Unwrap() error { return e.Err }
