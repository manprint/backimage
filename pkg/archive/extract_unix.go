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
	opts     ExtractOptions
	warnings []string
	warned   map[string]bool
	degraded map[string]int64
}

func extractorFor(opts ExtractOptions) Extractor {
	return &extractor{opts: opts, warned: make(map[string]bool), degraded: make(map[string]int64)}
}

// warn records a non-fatal degradation once per distinct cause: the same
// missing privilege repeats on every entry of a multi-gigabyte restore, and a
// single line is all the user needs.
func (x *extractor) warn(key, message string) {
	if x.warned[key] {
		return
	}
	x.warned[key] = true
	x.warnings = append(x.warnings, message)
	if x.opts.Progress != nil {
		x.opts.Progress("restore: attenzione: " + message)
	}
}

// degrade records one metadata operation that the destination refused. In
// strict mode the error is returned unchanged and the caller aborts; otherwise
// the class is counted, the cause is warned about once, and nil is returned so
// the entry keeps its content and the metadata that did apply.
//
// This is what makes a restore survive a heterogeneous tree: ownership, mode,
// timestamps, ACLs and extended attributes are all best-effort, and losing one
// of them is a reportable degradation, never a reason to stop.
func (x *extractor) degrade(class, key, message string, err error) error {
	if x.opts.Strict {
		return err
	}
	x.degraded[class]++
	x.warn(key, message)
	return nil
}

// Failures that are never degradations: they mean the request itself is
// refused (the caller must pass --overwrite) or the archive is not what this
// extractor can materialise. Skipping them would hide a real problem.
var (
	errNeedOverwrite    = errors.New("destinazione già esistente: usa --overwrite")
	errUnsupportedEntry = errors.New("tipo di entry non supportato")
)

// fatalAlways reports the failures that abort even in degraded mode.
func fatalAlways(err error) bool {
	return fatalFS(err) || errors.Is(err, errNeedOverwrite) ||
		errors.Is(err, errUnsupportedEntry) || errors.Is(err, io.ErrUnexpectedEOF)
}

// fatalFS reports the errors that mean the destination itself is unusable: no
// amount of degradation would make the next entry succeed, so aborting is the
// only honest answer even in degraded mode.
func fatalFS(err error) bool {
	for _, e := range []syscall.Errno{
		syscall.ENOSPC, syscall.EDQUOT, syscall.EROFS, syscall.EIO,
		syscall.ENOMEM, syscall.EMFILE, syscall.ENFILE,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
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
func (x *extractor) Extract(ctx context.Context, r io.Reader, dest string) (stats Stats, err error) {
	// Warnings are reported even when the extraction fails later on: they
	// explain what was already degraded before the failure.
	defer func() { stats.Warnings = x.warnings }()
	tr := tar.NewReader(r)
	dest = filepath.Clean(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return stats, fmt.Errorf("mkdir dest %q: %w", dest, err)
	}
	if x.opts.Progress != nil {
		x.opts.Progress("restore: filesystem: scrittura file e directory")
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
			// Degraded mode: only a broken destination or a broken archive
			// stops the run. Anything else costs one entry, not the restore.
			if x.opts.Strict || fatalAlways(err) {
				return stats, err
			}
			stats.Skipped++
			stats.Errors = append(stats.Errors, err)
			x.degraded["object"]++
			x.warn("object-skipped", "alcune entry non sono state create: la prima è "+err.Error()+
				" (elenco completo in Stats.Errors / --json)")
			continue
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
	if x.opts.Progress != nil {
		x.opts.Progress("restore: filesystem: finalizzazione metadati directory")
	}
	sort.Slice(dirFixes, func(i, j int) bool {
		return len(dirFixes[i].path) > len(dirFixes[j].path)
	})
	for _, d := range dirFixes {
		if err := os.Chmod(d.path, headerMode(d.hdr)); err != nil {
			if err := x.degrade("mode", "mode-dir", modeDegradeMsg, fmt.Errorf("chmod dir %q: %w", d.path, err)); err != nil {
				return stats, err
			}
		}
		at := unix.Timespec{Nsec: utimeOmit}
		if !d.at.IsZero() {
			at = unix.NsecToTimespec(d.at.UnixNano())
		}
		if !d.mt.IsZero() {
			ts := []unix.Timespec{
				at,
				unix.NsecToTimespec(d.mt.UnixNano()),
			}
			if err := unix.UtimesNanoAt(unix.AT_FDCWD, d.path, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if err := x.degrade("times", "times-dir", timesDegradeMsg, fmt.Errorf("utimes dir %q: %w", d.path, err)); err != nil {
					return stats, err
				}
			}
		}
	}
	stats.Degraded = x.degraded
	if x.opts.Progress != nil {
		x.opts.Progress("restore: filesystem: finalizzazione completata")
		x.opts.Progress("restore: " + summarize(stats))
	}
	return stats, nil
}

// summarize renders the one line that says what was lost, loudly when objects
// are missing and quietly when only metadata was degraded.
func summarize(stats Stats) string {
	if len(stats.Degraded) == 0 {
		return "nessuna degradazione: contenuti e metadati ripristinati integralmente"
	}
	classes := make([]string, 0, len(stats.Degraded))
	for class := range stats.Degraded {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	for _, class := range classes {
		parts = append(parts, fmt.Sprintf("%s=%d", class, stats.Degraded[class]))
	}
	line := "degradazioni: " + strings.Join(parts, " ") + " (dettaglio negli avvisi sopra)"
	if stats.Skipped > 0 {
		return fmt.Sprintf("ATTENZIONE: %d entry NON estratte. %s", stats.Skipped, line)
	}
	return "contenuto dei file integro, " + line
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
			return fmt.Errorf("%q già esistente: %w", target, errNeedOverwrite)
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
				return permHint("symlink "+hdr.Linkname, symlinkPermHint, err)
			}
			return fmt.Errorf("symlink %q: %w", target, err)
		}
		stats.Symlinks++
	case tar.TypeLink:
		// A hardlink whose first name cannot be linked again (different
		// device, filesystem without hardlinks, protected_hardlinks, or a
		// first name filtered out of this restore) is materialised as an
		// independent copy: the bytes matter more than the shared inode.
		first := filepath.Join(dest, filepath.FromSlash(hdr.Linkname))
		if err := os.Link(first, target); err != nil {
			if fatalFS(err) {
				return fmt.Errorf("hardlink %q -> %q: %w", target, first, err)
			}
			copied, cerr := copyFile(first, target, headerMode(hdr))
			if cerr != nil {
				return fmt.Errorf("hardlink %q -> %q: %w (copia di riserva: %w)", target, first, err, cerr)
			}
			if err := x.degrade("hardlink", "hardlink-copy", hardlinkDegradeMsg, err); err != nil {
				return err
			}
			stats.Files++
			stats.BytesRaw += copied
			break
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
				return permHint("mknod", nodePermHint, err)
			}
			return fmt.Errorf("mknod %q: %w", target, err)
		}
		stats.Devices++
	case tar.TypeFifo:
		if err := unix.Mkfifo(target, uint32(mode)); err != nil {
			if isPerm(err) {
				return permHint("mkfifo", nodePermHint, err)
			}
			return fmt.Errorf("mkfifo %q: %w", target, err)
		}
		stats.Fifos++
	default:
		return fmt.Errorf("%q: typeflag %q: %w", hdr.Name, hdr.Typeflag, errUnsupportedEntry)
	}

	// Order (mandatory, see docs/FIDELITY.md):
	//  3. lchown(uid, gid)  <- after creation, before chmod (chown clears setuid/setgid)
	//  4. chmod(mode)       <- not for symlinks (no lchmod on Linux)
	//  5. setxattr(...)     <- after chown (security.capability cleared by chown)
	//  6. utimes(atime, mtime) <-- last
	if x.opts.PreserveOwner {
		if err := unix.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
			wrapped := fmt.Errorf("lchown %q: %w", target, err)
			if isPerm(err) {
				wrapped = permHint("chown", ownerPermHint, err)
			}
			if err := x.degrade("owner", "owner", ownerDegradeMsg, wrapped); err != nil {
				return err
			}
		}
	}

	// chmod (not for symlinks). Directories are chmod'd in the final pass
	// (deepest-first), never here: a 0500 dir must be writable while its
	// children are being created.
	if hdr.Typeflag != tar.TypeSymlink && hdr.Typeflag != tar.TypeDir {
		if err := os.Chmod(target, headerMode(hdr)); err != nil {
			// Degraded mode drops the mode, not the remaining metadata of the
			// entry: fall through to the timestamps instead of returning.
			if err := x.degrade("mode", "mode", modeDegradeMsg, fmt.Errorf("chmod %q: %w", target, err)); err != nil {
				return err
			}
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
				// An attribute the destination cannot hold must not destroy the
				// restore: the file content is already written and verified.
				ns := xattrNamespace(rest)
				if key, message, tolerated := tolerateXattr(rest, err); tolerated {
					// Tolerated even in strict mode: nothing could have been
					// preserved here on this destination.
					x.warn(key, message)
					x.degraded["xattr."+ns]++
					stats.XattrsSkipped++
					continue
				}
				wrapped := fmt.Errorf("setxattr %q %s: %w", target, rest, err)
				if isPerm(err) {
					wrapped = permHint("setxattr "+rest, xattrPermHint, err)
				}
				if err := x.degrade("xattr."+ns, "xattr-"+ns, fmt.Sprintf(
					"xattr %s.* non applicabili sulla destinazione: ignorati", ns), wrapped); err != nil {
					return err
				}
				stats.XattrsSkipped++
			}
		}
	}
	// timestamps last (lutimes semantics: symlink-safe). atime is omitted
	// when the archive carries no value for it (UTIME_OMIT keeps the
	// extraction-time atime instead of clamping it to the epoch).
	at := unix.Timespec{Nsec: utimeOmit}
	if !hdr.AccessTime.IsZero() {
		at = unix.NsecToTimespec(hdr.AccessTime.UnixNano())
	}
	if !hdr.ModTime.IsZero() {
		ts := []unix.Timespec{
			at,
			unix.NsecToTimespec(hdr.ModTime.UnixNano()),
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, target, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if err := x.degrade("times", "times", timesDegradeMsg, fmt.Errorf("utimes %q: %w", target, err)); err != nil {
				return err
			}
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

func permHint(op, hint string, err error) error {
	return &PermissionHintError{Op: op, Hint: hint, Err: err}
}

// Remediations attached to the privilege failures a restore can hit. They are
// only ever shown in strict mode: without --strict these become degradations.
const (
	xattrPermHint = "esegui senza --strict per ignorare l'attributo, " +
		"oppure con privilegi (docker run --privileged, o --cap-add SYS_ADMIN)"
	ownerPermHint   = "esegui senza --strict, con --no-preserve-owner, oppure come root"
	nodePermHint    = "esegui senza --strict, oppure con privilegi (--cap-add MKNOD)"
	symlinkPermHint = "esegui senza --strict; la destinazione rifiuta i symlink"
)

// One line per degraded class, warned about once however many entries hit it.
const (
	ownerDegradeMsg = "owner/gruppo non ripristinabili su alcune entry: " +
		"restano dell'utente corrente (contenuti e nomi invariati)"
	modeDegradeMsg = "permessi non applicabili su alcune entry: " +
		"resta il mode di creazione (contenuti invariati)"
	timesDegradeMsg = "timestamp non applicabili su alcune entry: " +
		"resta l'ora di estrazione (contenuti invariati)"
	hardlinkDegradeMsg = "hardlink non ricreabili su questa destinazione: " +
		"materializzati come copie indipendenti (nessun byte perso, spazio su disco maggiore)"
)

// copyFile duplicates src into dst, used as the hardlink fallback. It returns
// the number of bytes written.
func copyFile(src, dst string, mode fs.FileMode) (int64, error) {
	in, err := os.Open(src) // #nosec G304 -- src is a path already materialised under dest
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return 0, err
	}
	return n, nil
}

// xattrNamespace returns the leading namespace of an extended attribute name
// ("trusted", "security", "system", "user"), or "" when there is none.
func xattrNamespace(name string) string {
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return ""
}

// tolerateXattr reports whether a failed Lsetxattr must be downgraded to a
// warning instead of aborting the restore, and returns a dedup key plus the
// message to record.
//
// Two families are tolerated even in strict mode:
//
//   - trusted.*: writing that namespace requires CAP_SYS_ADMIN in the initial
//     user namespace. A container started without --privileged never has it,
//     and what actually lives there is overlayfs bookkeeping of the archived
//     tree (trusted.overlay.opaque/redirect/origin), not user data. This is
//     the common case when the backup contains a nested /var/lib/docker.
//   - namespaces the destination filesystem refuses outright (EOPNOTSUPP on
//     tmpfs/NFS/vfat, EINVAL for a prefix the kernel does not know).
//
// security.*, system.* (ACLs) and user.* keep honouring Strict: they carry
// real data, and losing them silently would be a fidelity bug.
func tolerateXattr(name string, err error) (key, message string, tolerated bool) {
	ns := xattrNamespace(name)
	switch {
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EINVAL):
		return "xattr-unsupported-" + ns, fmt.Sprintf(
			"xattr %s.* non supportati dal filesystem di destinazione: ignorati "+
				"(i dati dei file non sono interessati)", ns), true
	case ns == "trusted" && isPerm(err):
		return "xattr-trusted-eperm", "xattr trusted.* non ripristinabili senza CAP_SYS_ADMIN: " +
			"ignorati (metadati interni di overlayfs, i dati dei file non sono interessati)", true
	}
	return "", "", false
}

// PermissionHintError carries a user-facing remediation for privilege failures.
type PermissionHintError struct {
	Op   string
	Hint string
	Err  error
}

func (e *PermissionHintError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %v (%s)", e.Op, e.Err, e.Hint)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *PermissionHintError) Unwrap() error { return e.Err }
