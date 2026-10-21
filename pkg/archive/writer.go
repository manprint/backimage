//go:build unix || windows

package archive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manprint/backimage/internal/pathglob"
)

// hardlinkKey identifies an inode for hardlink grouping.
type hardlinkKey struct {
	Dev uint64
	Ino uint64
}

type tarWriter struct {
	tw        *tar.Writer
	w         io.Writer
	opts      Options
	stats     Stats
	entries   []Entry
	links     map[hardlinkKey]string
	rootBases map[string]string // basename -> root, to detect collisions
	devSeen   uint64            // device of the first root (OneFileSystem)
	devSet    bool
	written   int64 // total bytes emitted into the archive
}

func newWriter(w io.Writer, opts Options) *tarWriter {
	tw := &tarWriter{
		w:         w,
		opts:      opts,
		links:     map[hardlinkKey]string{},
		rootBases: map[string]string{},
	}
	tw.tw = tar.NewWriter(&counterWriter{w: w, n: &tw.written})
	if opts.PreserveXattrs && !xattrsSupported {
		// Not a failure: the platform has no extended attributes, so there is
		// nothing to preserve and nothing to lose. Said once, not per entry.
		tw.warnOnce("questa piattaforma non ha attributi estesi: nessuno è stato archiviato")
	}
	return tw
}

// warnOnce records a non-fatal degradation, at most one line per distinct
// cause: the same missing capability on ten thousand entries is one fact.
func (w *tarWriter) warnOnce(msg string) {
	for _, seen := range w.stats.Warnings {
		if seen == msg {
			return
		}
	}
	w.stats.Warnings = append(w.stats.Warnings, msg)
}

// counterWriter forwards writes while maintaining the emitted offset.
type counterWriter struct {
	w io.Writer
	n *int64
}

func (c *counterWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if c.n != nil {
		*c.n += int64(n)
	}
	return n, err
}

// deviceOf reports the filesystem device an entry lives on. It is a variable
// so a test can place a mount boundary inside a tree without mounting
// anything; everything else about the walk stays real.
var deviceOf = fileDevice

func (w *tarWriter) AddRoot(ctx context.Context, root string) error {
	root = filepath.Clean(root)
	base := filepath.Base(root)
	if prev, ok := w.rootBases[base]; ok {
		return &RootCollisionError{Base: base, A: prev, B: root}
	}
	w.rootBases[base] = root
	if w.excluded(base) {
		w.stats.Skipped++
		return nil
	}

	st, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("lstat root %q: %w", root, err)
	}
	if w.opts.OneFileSystem && !w.devSet {
		if dev, ok := deviceOf(st); ok {
			w.devSeen = dev
			w.devSet = true
		}
	}
	// A later root on another filesystem is not archived at all: the option
	// says one file system, and the first root is the one that names it.
	if w.opts.OneFileSystem && w.devSet {
		if dev, ok := deviceOf(st); ok && dev != w.devSeen {
			w.stats.Skipped++
			return nil
		}
	}
	src := source{name: base, path: root}
	if st.IsDir() {
		l, err := src.list(st)
		if err != nil {
			// Strict mode: fail before emitting anything if the root cannot
			// be read at all, so an unreadable tree never produces a partial
			// tar.
			if w.opts.Strict {
				return w.handleWalkError(fmt.Errorf("readdir root %q: %w", root, err), root)
			}
			if err := w.emitOne(ctx, base, src, st, nil); err != nil {
				return err
			}
			return w.handleWalkError(fmt.Errorf("readdir %q: %w", root, err), root)
		}
		defer l.close()
		if err := w.emitOne(ctx, base, src, st, l.file); err != nil {
			return err
		}
		l.releaseFile()
		return w.walkChildren(ctx, base, root, root, l)
	}
	return w.emitOne(ctx, base, src, st, nil)
}

// RootCollisionError reports two roots with the same basename.
type RootCollisionError struct {
	Base string
	A, B string
}

// Hint carries the remediation shown to the user.
func (e *RootCollisionError) Hint() string {
	return "archives would store both trees at the same archive path; use --strip-prefix or rename one root"
}

func (e *RootCollisionError) Error() string {
	return fmt.Sprintf("roots %q and %q share the basename %q", e.A, e.B, e.Base)
}

// walkChildren archives the entries of the directory l, in name order, each
// subtree complete before the next sibling: the order is deterministic and a
// directory always precedes its content. fsRoot is the root being archived,
// dirPath the filesystem path of l.
//
// The walk holds one open directory per level of depth and resolves every
// child against its parent's handle, never by path (see source).
func (w *tarWriter) walkChildren(ctx context.Context, arcDir, fsRoot, dirPath string, l *listing) error {
	for _, name := range l.names {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("archiving %q: %w", fsRoot, err)
		}
		src := source{dir: l.dir, name: name, path: filepath.Join(dirPath, name)}
		if err := w.walkEntry(ctx, arcDir+"/"+name, fsRoot, src); err != nil {
			return err
		}
	}
	return nil
}

func (w *tarWriter) walkEntry(ctx context.Context, rel, fsRoot string, src source) error {
	// Decided on the name alone, before anything is stat'ed, opened or
	// listed: nothing at or below an excluded path is touched.
	if w.excluded(rel) {
		w.stats.Skipped++
		return nil
	}
	st, err := src.lstat()
	if err != nil {
		return w.handleWalkError(fmt.Errorf("lstat %q: %w", src.path, err), src.path)
	}
	if w.opts.OneFileSystem && w.devSet {
		if dev, ok := deviceOf(st); ok && dev != w.devSeen {
			// Another device means a mount point. The directory itself is
			// archived and nothing below it is visited — the same as
			// `tar --one-file-system` and `rsync -x`. Dropping the
			// directory too, which is what happened before, restored a
			// tree with nowhere to mount anything back: a /srv/data with
			// no /srv/data/db to remount into.
			//
			// Anything else on another device — a bind-mounted file — is
			// left out entirely; there is no shape to preserve.
			//
			// One Skipped per boundary. The entries below it are never
			// enumerated, which is the point of the option, so their
			// number is not knowable without doing the walk it exists to
			// avoid.
			if st.IsDir() {
				// The mount point's own attributes are read through a
				// checked descriptor when one can be had; its content is
				// never listed.
				dirFile, err := src.openVerified(st)
				if err != nil {
					dirFile = nil
				} else {
					defer dirFile.Close()
				}
				if err := w.emitOne(ctx, rel, src, st, dirFile); err != nil {
					return err
				}
			}
			w.stats.Skipped++
			return nil
		}
	}
	if !st.IsDir() {
		return w.emitOne(ctx, rel, src, st, nil)
	}
	l, err := src.list(st)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			// Unreadable directory (e.g. 0500): archive the dir itself
			// and skip its contents; not an abort-worthy error.
			if err2 := w.emitOne(ctx, rel, src, st, nil); err2 != nil {
				return err2
			}
			w.stats.Skipped++
			return nil
		}
		return w.handleWalkError(fmt.Errorf("readdir %q: %w", src.path, err), src.path)
	}
	defer l.close()
	// Emit the dir first, then children (deterministic order: dir before content).
	if err := w.emitOne(ctx, rel, src, st, l.file); err != nil {
		return err
	}
	l.releaseFile()
	return w.walkChildren(ctx, rel, fsRoot, src.path, l)
}

// emitOne archives the entry src, which st describes. dirFile is the checked
// descriptor of a directory entry, or nil.
func (w *tarWriter) emitOne(ctx context.Context, arcPath string, src source, st os.FileInfo, dirFile *os.File) error {
	fsPath := src.path
	// Excluded entries are not opened or read at all: an exclude is also how
	// a user keeps the backup away from a file it must not touch.
	if w.excluded(arcPath) {
		w.stats.Skipped++
		return nil
	}
	e := &Entry{Path: arcPath}
	mode := st.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		e.Type = TypeSymlink
		tgt, err := src.readlink()
		if err != nil {
			return w.handleWalkError(fmt.Errorf("readlink %q: %w", fsPath, err), fsPath)
		}
		e.LinkTarget = tgt
	case mode.IsRegular():
		e.Type = TypeRegular
		e.Size = st.Size()
	case mode.IsDir():
		e.Type = TypeDir
	case mode&os.ModeCharDevice != 0:
		e.Type = TypeCharDevice
	case mode&os.ModeDevice != 0:
		e.Type = TypeBlockDevice
	case mode&os.ModeNamedPipe != 0:
		e.Type = TypeFifo
	case mode&os.ModeSocket != 0:
		w.stats.Skipped++
		return nil
	default:
		return w.handleWalkError(fmt.Errorf("%q: unsupported file type %v", fsPath, mode), fsPath)
	}
	e.Mode = mode.Perm()
	if mode&os.ModeSetuid != 0 {
		e.Mode |= os.ModeSetuid
	}
	if mode&os.ModeSetgid != 0 {
		e.Mode |= os.ModeSetgid
	}
	if mode&os.ModeSticky != 0 {
		e.Mode |= os.ModeSticky
	}
	// A regular file is opened, and checked to be the file st describes,
	// before anything else is read from it: its attributes then come from
	// the same descriptor as its content.
	var f *os.File
	var openErr error
	if e.Type == TypeRegular {
		f, openErr = src.openVerified(st)
		if openErr == nil {
			defer f.Close()
		}
	}
	metaFile := f
	if e.Type == TypeDir {
		metaFile = dirFile
	}
	if err := readMeta(src, metaFile, st, w.opts, e); err != nil {
		var lost *xattrLossError
		if !errors.As(err, &lost) || w.opts.Strict {
			return w.handleWalkError(fmt.Errorf("metadata %q: %w", fsPath, err), fsPath)
		}
		// Degraded mode: keep the entry, count the attributes as lost. A file
		// nobody can list the attributes of is still a file worth archiving.
		w.stats.XattrsSkipped++
		w.stats.Errors = append(w.stats.Errors, err)
		w.warnOnce("attributi estesi non leggibili su alcune entry: sono stati esclusi dall'archivio")
	}
	return w.writeEntry(ctx, e, fsPath, st, f, openErr)
}

func (w *tarWriter) excluded(arcPath string) bool { return excludedBy(w.opts.Excludes, arcPath) }

// excludedBy reports whether an archive path, or one of its ancestors,
// matches one of the exclude patterns. The estimate and preflight walks use
// it too, so all three agree on what the backup contains; restore, ls and
// find apply the same pathglob.MatchTree rule.
//
// The ancestor rule matters even though the walks stop at an excluded
// directory: it is what makes "**/.cache" and "alice/.cache" mean the same
// subtree. Only literal patterns used to cover descendants, so a glob that
// named a directory dropped the directory entry and archived its content.
func excludedBy(patterns []string, arcPath string) bool {
	return pathglob.MatchTreeAny(patterns, arcPath)
}

func (w *tarWriter) handleWalkError(err error, _ string) error {
	if w.opts.Strict {
		return err
	}
	w.stats.Errors = append(w.stats.Errors, err)
	w.stats.Skipped++
	return nil
}

// writeEntry emits the header of e and, for a regular file, its content from
// f. openErr is why a regular file has no f.
func (w *tarWriter) writeEntry(ctx context.Context, e *Entry, fsPath string, st os.FileInfo, f *os.File, openErr error) error {
	hdr := &tar.Header{
		Name:       e.Path,
		Mode:       int64(e.Mode.Perm()),
		Uid:        e.UID,
		Gid:        e.GID,
		Uname:      e.Uname,
		Gname:      e.Gname,
		ModTime:    e.ModTime,
		AccessTime: e.AccessTime,
		ChangeTime: e.ChangeTime,
		Format:     tar.FormatPAX,
		PAXRecords: map[string]string{},
	}
	if e.Mode&os.ModeSetuid != 0 {
		hdr.Mode |= 1 << 11
	}
	if e.Mode&os.ModeSetgid != 0 {
		hdr.Mode |= 1 << 10
	}
	if e.Mode&os.ModeSticky != 0 {
		hdr.Mode |= 1 << 9
	}
	// PAX records: SCHILY.xattr.<name> for raw xattr values (binary-safe,
	// length-prefixed). atime/ctime are emitted by the stdlib writer itself
	// (tar.Header.AccessTime/ChangeTime) under FormatPAX, but only when
	// PreserveTimes opts in — otherwise the archive stays byte-deterministic.
	for k, v := range e.Xattrs {
		if w.opts.PreserveXattrs {
			hdr.PAXRecords["SCHILY.xattr."+k] = string(v)
		}
	}
	if !w.opts.PreserveTimes {
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}
	}
	if e.Type == TypeRegular {
		if key, ok := fileIdentity(st); ok {
			if first, seen := w.links[key]; seen {
				e.Type = TypeHardlink
				e.LinkTarget = first
				e.Size = 0
				hdr.Typeflag = tar.TypeLink
				hdr.Linkname = first
				hdr.Size = 0
			} else {
				w.links[key] = e.Path
			}
		}
	}
	switch e.Type {
	case TypeDir:
		hdr.Typeflag = tar.TypeDir
		hdr.Name = strings.TrimSuffix(e.Path, "/") + "/"
	case TypeSymlink:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = e.LinkTarget
	case TypeHardlink:
		hdr.Typeflag = tar.TypeLink
		hdr.Linkname = e.LinkTarget
		e.Size = 0
	case TypeCharDevice:
		hdr.Typeflag = tar.TypeChar
		hdr.Devmajor = e.DevMajor
		hdr.Devminor = e.DevMinor
	case TypeBlockDevice:
		hdr.Typeflag = tar.TypeBlock
		hdr.Devmajor = e.DevMajor
		hdr.Devminor = e.DevMinor
	case TypeFifo:
		hdr.Typeflag = tar.TypeFifo
	case TypeRegular:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = e.Size
	}
	// Strict mode: open regular files BEFORE emitting the header, so an
	// unreadable file produces an error before any part of its entry lands
	// in the tar (no partial archive with dangling headers).
	if e.Type == TypeRegular && f == nil {
		err := openErr
		if err == nil {
			err = errors.New("no descriptor")
		}
		if w.opts.Strict {
			return w.handleWalkError(fmt.Errorf("open %q: %w", fsPath, err), fsPath)
		}
		// Degraded mode: skip the payload, keep the metadata entry as an
		// empty regular file (a header without matching size would be a
		// corrupt tar). Content mismatch is reported to the caller.
		w.stats.Errors = append(w.stats.Errors, fmt.Errorf("open %q: %w", fsPath, err))
		w.stats.ContentSkipped++
		w.warnOnce("contenuto di alcuni file non leggibile: archiviati come file vuoti, " +
			"con nome, permessi, owner e timestamp conservati (--allow-degraded)")
		e.Size = 0
		hdr.Size = 0
	}
	// archive/tar may defer the padding of the previous regular file until
	// WriteHeader. Record the logical start of the next header, not the number
	// of bytes which happened to reach the underlying writer so far.
	e.TarOffset = (w.written + 511) &^ int64(511)
	if err := w.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %q: %w", e.Path, err)
	}
	if e.Type == TypeRegular {
		// The digest covers what actually reaches the archive, not what was
		// on disk: in degraded mode f is nil and the payload is empty, so
		// this is the digest of no bytes. Computing it only when f != nil
		// used to leave SHA256 empty on those entries, and the index schema
		// rejects a regular entry without a digest — an unreadable file made
		// the whole backup fail at the metadata step, after the archive had
		// already been built, compressed and encrypted. Stats.ContentSkipped
		// is what says the bytes are missing; the digest stays truthful about
		// the archive.
		//
		// The header already promised e.Size bytes, and tar holds a writer to
		// it: a file that grew while it was read is cut at that size, one that
		// shrank is padded with zeros, the way GNU tar does. Copying whatever
		// the file held used to fail with "write too long" or, on the next
		// header, "missed writing" — the whole backup lost to one log written
		// while it was archived, in the mode meant to tolerate a live source.
		h := sha256.New()
		var n int64
		grew := false
		if f != nil {
			src := &ctxReader{ctx: ctx, r: f}
			var err error
			n, err = io.Copy(io.MultiWriter(w.tw, h), io.LimitReader(src, e.Size))
			if err != nil {
				return fmt.Errorf("copy %q: %w", fsPath, err)
			}
			if n == e.Size {
				var probe [1]byte
				m, err := src.Read(probe[:])
				if err != nil && !errors.Is(err, io.EOF) {
					return fmt.Errorf("copy %q: %w", fsPath, err)
				}
				grew = m > 0
			}
		}
		if n != e.Size || grew {
			read := fmt.Sprint(n)
			if grew {
				read = fmt.Sprintf("more than %d", n)
			}
			msg := fmt.Errorf("size changed while archiving %q: stat %d, read %s", fsPath, e.Size, read)
			if w.opts.Strict {
				return msg
			}
			w.stats.Errors = append(w.stats.Errors, msg)
			w.warnOnce("alcuni file sono cambiati mentre venivano letti: archiviati alla dimensione rilevata " +
				"all'inizio (troncati o completati con zeri)")
			if pad := e.Size - n; pad > 0 {
				if _, err := io.CopyN(io.MultiWriter(w.tw, h), zeroReader{}, pad); err != nil {
					return fmt.Errorf("pad %q: %w", fsPath, err)
				}
			}
		}
		e.SHA256 = hex.EncodeToString(h.Sum(nil))
		w.stats.BytesRaw += n
	}
	switch e.Type {
	case TypeRegular:
		w.stats.Files++
	case TypeDir:
		w.stats.Dirs++
	case TypeSymlink:
		w.stats.Symlinks++
	case TypeHardlink:
		w.stats.Hardlinks++
	case TypeCharDevice, TypeBlockDevice:
		w.stats.Devices++
	case TypeFifo:
		w.stats.Fifos++
	}
	w.entries = append(w.entries, *e)
	return nil
}

func (w *tarWriter) Close() (Stats, error) {
	if err := w.tw.Close(); err != nil {
		return w.stats, fmt.Errorf("tar trailer: %w", err)
	}
	return w.stats, nil
}

func (w *tarWriter) Entries() []Entry { return w.entries }

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

// zeroReader yields zero bytes forever.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func (c *ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	return c.r.Read(p)
}
