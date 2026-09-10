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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type extractor struct {
	opts     ExtractOptions
	dest     string
	warnings []string
	warned   map[string]bool
	degraded map[string]int64
	examples map[string]string
}

// show renders an archive-relative name the way the user typed the
// destination. Every filesystem operation below works on the relative name
// through an os.Root; only messages get to see the whole path.
func (x *extractor) show(name string) string {
	return filepath.Join(x.dest, filepath.FromSlash(name))
}

func extractorFor(opts ExtractOptions) Extractor {
	return &extractor{
		opts:     opts,
		warned:   make(map[string]bool),
		degraded: make(map[string]int64),
		examples: make(map[string]string),
	}
}

// note records one degradation of class, keeping the first real failure as the
// evidence reported at the end of the extraction.
func (x *extractor) note(class string, err error) {
	x.degraded[class]++
	if err != nil && x.examples[class] == "" {
		x.examples[class] = err.Error()
	}
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
	x.note(class, err)
	x.warn(key, message)
	return nil
}

// Failures that are never degradations: they mean the request itself is
// refused (the caller must pass --overwrite) or the archive is not what this
// extractor can materialise. Skipping them would hide a real problem.
var (
	errNeedOverwrite    = errors.New("destinazione già esistente: usa --overwrite")
	errUnsupportedEntry = errors.New("tipo di entry non supportato")
	// errLinkTargetNotRestored is a per-entry skip, not a fatal error: the
	// archive is intact, this restore just did not produce the file the link
	// points at. See DA-03 and the tar.TypeLink case in createOne.
	errLinkTargetNotRestored = errors.New(
		"il primo nome dell'hardlink non è stato ripristinato in questa corsa: entry saltata")
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
	x.dest = dest
	// Every path below is resolved through this root and never rebuilt into a
	// string that is then reopened. os.Root anchors each step of the walk to a
	// directory descriptor, so the check and the use are the same syscall: a
	// component replaced by a symlink between them cannot move the operation
	// outside dest, because there is no "between them" left.
	//
	// The validation this replaces returned a pathname. Every operation
	// afterwards — create, chmod, chown, xattr, timestamps, and the final
	// directory pass — resolved that pathname again, and a concurrent
	// modification of the tree invalidated the check without invalidating the
	// string. An extra Lstat before each of them would not have closed it.
	root, err := os.OpenRoot(dest)
	if err != nil {
		return stats, fmt.Errorf("open dest %q: %w", dest, err)
	}
	defer root.Close()
	if x.opts.Progress != nil {
		x.opts.Progress("restore: filesystem: scrittura file e directory")
	}
	type dirFix struct {
		name string
		hdr  *tar.Header
		at   time.Time
		mt   time.Time
	}
	var dirFixes []dirFix
	// materialised holds the archive names of the regular files this run has
	// actually written. It is the only thing a hardlink is allowed to point
	// at; see the tar.TypeLink case in createOne.
	materialised := make(map[string]bool)

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
		if !selects(x.opts, name) {
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
				// --strip-components cut the first name away entirely. The
				// link has nothing left to point at, and dropping it without
				// a word would make the restore look complete.
				err := fmt.Errorf("hardlink %q: %w", name, errLinkTargetNotRestored)
				if x.opts.Strict {
					return stats, err
				}
				stats.Skipped++
				stats.Errors = append(stats.Errors, err)
				x.note("object", err)
				continue
			}
			hdr.Linkname = link
		}
		if err := checkArchivePath(name); err != nil {
			// A hostile name used to vanish without a trace outside strict
			// mode. It is an entry that was not created, like any other.
			if x.opts.Strict {
				return stats, err
			}
			stats.Skipped++
			stats.Errors = append(stats.Errors, err)
			x.note("object", err)
			continue
		}
		if err := x.createOne(ctx, root, name, hdr, tr, materialised, &stats); err != nil {
			// Degraded mode: only a broken destination or a broken archive
			// stops the run. Anything else costs one entry, not the restore.
			if x.opts.Strict || fatalAlways(err) {
				return stats, err
			}
			stats.Skipped++
			stats.Errors = append(stats.Errors, err)
			x.note("object", err)
			x.warn("object-skipped", "alcune entry non sono state create: la prima è "+err.Error()+
				" (elenco completo in Stats.Errors / --json)")
			continue
		}
		if hdr.Typeflag == tar.TypeDir {
			dirFixes = append(dirFixes, dirFix{
				name: name,
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
		return len(dirFixes[i].name) > len(dirFixes[j].name)
	})
	for _, d := range dirFixes {
		if err := root.Chmod(d.name, headerMode(d.hdr)); err != nil {
			if err := x.degrade("mode", "mode-dir", modeDegradeMsg,
				fmt.Errorf("chmod dir %q: %w", x.show(d.name), err)); err != nil {
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
			if err := utimesIn(root, d.name, ts); err != nil {
				if err := x.degrade("times", "times-dir", timesDegradeMsg,
					fmt.Errorf("utimes dir %q: %w", x.show(d.name), err)); err != nil {
					return stats, err
				}
			}
		}
	}
	stats.Degraded = x.degraded
	stats.DegradedExamples = x.examples
	if x.opts.Progress != nil {
		x.opts.Progress("restore: filesystem: finalizzazione completata")
		for _, line := range stats.FidelityLines() {
			x.opts.Progress("restore: " + line)
		}
	}
	return stats, nil
}

// openParent opens the directory holding name, through root, and returns it
// with the base name.
//
// The descriptor is what the *at syscalls need. os.Root covers neither mknod
// nor the AT_SYMLINK_NOFOLLOW form of utimensat, and handing those a rebuilt
// pathname would put back exactly the check-then-use gap os.Root is here to
// close.
func openParent(root *os.Root, name string) (*os.File, string, error) {
	dir, base := path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "."
	}
	f, err := root.OpenFile(dir, os.O_RDONLY, 0)
	if err != nil {
		return nil, "", err
	}
	return f, base, nil
}

// utimesIn applies timestamps to name without following a final symlink.
func utimesIn(root *os.Root, name string, ts []unix.Timespec) error {
	parent, base, err := openParent(root, name)
	if err != nil {
		return err
	}
	defer parent.Close()
	return unix.UtimesNanoAt(int(parent.Fd()), base, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func (x *extractor) createOne(
	ctx context.Context,
	root *os.Root,
	name string,
	hdr *tar.Header,
	tr *tar.Reader,
	materialised map[string]bool,
	stats *Stats,
) error {
	shown := x.show(name)

	// Overwrite handling.
	//
	// --overwrite overlays the archive onto the destination; it does not
	// replace the destination tree. The difference matters for directories:
	// RemoveAll on a directory that already exists deletes everything under
	// it, including files the backup never contained, so restoring one
	// selected subtree into a populated directory used to be a silent delete
	// of its siblings. Two directories with the same name are the same
	// directory — that is what `tar -x` does — and the metadata pass at the
	// end applies the archived owner, mode and timestamps to it anyway.
	//
	// Everything else is still removed first: a symlink, a device or a fifo
	// cannot be created over an existing name, and a regular file replacing a
	// regular file is a replacement, not a merge.
	if existing, err := root.Lstat(name); err == nil {
		if !x.opts.Overwrite {
			return fmt.Errorf("%q già esistente: %w", shown, errNeedOverwrite)
		}
		if !(existing.IsDir() && hdr.Typeflag == tar.TypeDir) {
			if err := root.RemoveAll(name); err != nil {
				return fmt.Errorf("remove existing %q: %w", shown, err)
			}
			// The name no longer holds what this run put there, so it stops
			// being a legal hardlink target until something recreates it.
			delete(materialised, name)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %q: %w", shown, err)
	}

	// Intermediate dirs may be missing in manipulated archives: create them
	// 0700, the final chmod pass re-fixes them. Directories need their parents
	// too, and MkdirAll on the entry itself takes care of that below.
	if parent := path.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			if isPerm(err) {
				return permError("mkdir parent "+shown, err)
			}
			return fmt.Errorf("mkdir parent %q: %w", shown, err)
		}
	}

	// One descriptor for the containing directory, held for the whole entry.
	// It serves the calls os.Root does not cover — node creation and the
	// symlink-safe timestamps — and costs one open per entry.
	parent, base, err := openParent(root, name)
	if err != nil {
		return fmt.Errorf("open parent of %q: %w", shown, err)
	}
	defer parent.Close()
	dirfd := int(parent.Fd())

	mode := fs.FileMode(uint32(hdr.Mode)) & fs.ModePerm // #nosec G115 -- mode is 12 bits
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(name, 0o700); err != nil {
			if isPerm(err) {
				return permError("mkdir "+shown, err)
			}
			return fmt.Errorf("mkdir %q: %w", shown, err)
		}
		stats.Dirs++
	case tar.TypeReg:
		// O_TRUNC belongs here even though the overwrite pass above removed
		// any existing regular file: it is the one line that keeps this
		// correct if that pass ever stops removing, instead of leaving the
		// tail of a longer previous file behind the new content.
		f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return fmt.Errorf("create %q: %w", shown, err)
		}
		if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
			f.Close()
			return fmt.Errorf("write %q: %w", shown, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %q: %w", shown, err)
		}
		stats.Files++
		stats.BytesRaw += hdr.Size
		materialised[name] = true
	case tar.TypeSymlink:
		if err := root.Symlink(hdr.Linkname, name); err != nil {
			if isPerm(err) {
				return permHint("symlink "+hdr.Linkname, symlinkPermHint, err)
			}
			return fmt.Errorf("symlink %q: %w", shown, err)
		}
		stats.Symlinks++
	case tar.TypeLink:
		// A hardlink may only point at a regular file this same run has
		// already written.
		//
		// hdr.Linkname never went through the checks applied to hdr.Name, so
		// it used to be joined to the destination and linked as-is:
		// Linkname="../outside" gave the destination a second name for a file
		// outside it, and the metadata pass then rewrote that file's owner,
		// mode and timestamps through the shared inode — no privileges
		// required. The copy fallback opened the same arbitrary pathname.
		//
		// DA-03: a first name that was filtered out, cut by
		// --strip-components, or simply appears later in the archive is not
		// linkable. The entry is skipped and reported rather than
		// reconstructed by reading whatever happens to sit at that path.
		if !materialised[hdr.Linkname] {
			return fmt.Errorf("hardlink %q -> %q: %w", shown, hdr.Linkname, errLinkTargetNotRestored)
		}
		if err := root.Link(hdr.Linkname, name); err != nil {
			if fatalFS(err) {
				return fmt.Errorf("hardlink %q -> %q: %w", shown, hdr.Linkname, err)
			}
			// A hardlink that cannot be linked again (different device,
			// filesystem without hardlinks, protected_hardlinks) is
			// materialised as an independent copy of a file this run wrote:
			// the bytes matter more than the shared inode.
			copied, cerr := copyFileIn(root, hdr.Linkname, name, headerMode(hdr))
			if cerr != nil {
				return fmt.Errorf("hardlink %q -> %q: %w (copia di riserva: %w)", shown, hdr.Linkname, err, cerr)
			}
			if err := x.degrade("hardlink", "hardlink-copy", hardlinkDegradeMsg, err); err != nil {
				return err
			}
			stats.Files++
			stats.BytesRaw += copied
			materialised[name] = true
			break
		}
		stats.Hardlinks++
		// Another name for the same regular file: a later hardlink may point
		// at this one.
		materialised[name] = true
	case tar.TypeChar, tar.TypeBlock:
		typ := uint32(unix.S_IFCHR)
		if hdr.Typeflag == tar.TypeBlock {
			typ = unix.S_IFBLK
		}
		dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))                   // #nosec G115 -- devmajor/minor are 32-bit in kernel
		if err := mknodAt(dirfd, base, shown, typ|uint32(mode), int(dev)); err != nil { // #nosec G115 -- dev is a kernel rdev, fit in int
			if isPerm(err) {
				return permHint("mknod", nodePermHint, err)
			}
			return fmt.Errorf("mknod %q: %w", shown, err)
		}
		stats.Devices++
	case tar.TypeFifo:
		if err := mkfifoAt(dirfd, base, shown, uint32(mode)); err != nil {
			if isPerm(err) {
				return permHint("mkfifo", nodePermHint, err)
			}
			return fmt.Errorf("mkfifo %q: %w", shown, err)
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
		if err := root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
			wrapped := fmt.Errorf("lchown %q: %w", shown, err)
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
		if err := root.Chmod(name, headerMode(hdr)); err != nil {
			// Degraded mode drops the mode, not the remaining metadata of the
			// entry: fall through to the timestamps instead of returning.
			if err := x.degrade("mode", "mode", modeDegradeMsg, fmt.Errorf("chmod %q: %w", shown, err)); err != nil {
				return err
			}
		}
	}
	// xattrs after chown (capabilities are cleared by chown).
	if err := x.applyXattrs(root, name, shown, hdr, stats); err != nil {
		return err
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
		if err := unix.UtimesNanoAt(dirfd, base, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if err := x.degrade("times", "times", timesDegradeMsg, fmt.Errorf("utimes %q: %w", shown, err)); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyXattrs writes the SCHILY.xattr.* records of hdr onto the object just
// created, through a descriptor rather than a pathname.
//
// Only regular files and directories can be served this way: there is no
// *at form of setxattr, so the object has to be opened, and opening a symlink
// to write to it is impossible while opening a device node has side effects on
// the device itself. Those entries report their attributes as skipped instead
// of pretending. See docs/FIDELITY.md.
func (x *extractor) applyXattrs(root *os.Root, name, shown string, hdr *tar.Header, stats *Stats) error {
	if !x.opts.PreserveXattrs || hdr.PAXRecords == nil {
		return nil
	}
	pairs := make([][2]string, 0, len(hdr.PAXRecords))
	for k, v := range hdr.PAXRecords {
		if rest, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			pairs = append(pairs, [2]string{rest, v})
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeLink {
		x.warn("xattr-unopenable", "xattr non applicabili su symlink, device e fifo: ignorati "+
			"(non esiste una forma *at di setxattr e questi oggetti non si possono aprire senza effetti)")
		for range pairs {
			x.note("xattr.unopenable", nil)
			stats.XattrsSkipped++
		}
		return nil
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		wrapped := fmt.Errorf("open %q for xattrs: %w", shown, err)
		if isPerm(err) {
			wrapped = permHint("setxattr", xattrPermHint, err)
		}
		if err := x.degrade("xattr", "xattr-open", "xattr non applicabili: la destinazione "+
			"non consente di riaprire l'oggetto appena creato", wrapped); err != nil {
			return err
		}
		stats.XattrsSkipped += int64(len(pairs))
		return nil
	}
	defer f.Close()
	for _, kv := range pairs {
		attr, value := kv[0], kv[1]
		if err := unix.Fsetxattr(int(f.Fd()), attr, []byte(value), 0); err != nil {
			// An attribute the destination cannot hold must not destroy the
			// restore: the file content is already written and verified.
			ns := xattrNamespace(attr)
			if key, message, tolerated := tolerateXattr(attr, err); tolerated {
				// Tolerated even in strict mode: nothing could have been
				// preserved here on this destination.
				x.warn(key, message)
				x.note("xattr."+ns, fmt.Errorf("setxattr %q %s: %w", shown, attr, err))
				stats.XattrsSkipped++
				continue
			}
			wrapped := fmt.Errorf("setxattr %q %s: %w", shown, attr, err)
			if isPerm(err) {
				wrapped = permHint("setxattr "+attr, xattrPermHint, err)
			}
			if err := x.degrade("xattr."+ns, "xattr-"+ns, fmt.Sprintf(
				"xattr %s.* non applicabili sulla destinazione: ignorati", ns), wrapped); err != nil {
				return err
			}
			stats.XattrsSkipped++
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

// copyFileIn duplicates src into dst, both resolved through root, and returns
// the number of bytes written. It is the hardlink fallback, and src is always
// a name this run has already restored.
func copyFileIn(root *os.Root, src, dst string, mode fs.FileMode) (int64, error) {
	in, err := root.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := root.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rerr := root.Remove(dst); rerr != nil {
			return 0, errors.Join(err, rerr)
		}
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
