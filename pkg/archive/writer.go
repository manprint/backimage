//go:build unix

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
	"sort"
	"strings"
	"syscall"
	"time"
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
	return tw
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

func (w *tarWriter) AddRoot(ctx context.Context, root string) error {
	root = filepath.Clean(root)
	base := filepath.Base(root)
	if prev, ok := w.rootBases[base]; ok {
		return &RootCollisionError{Base: base, A: prev, B: root}
	}
	w.rootBases[base] = root

	st, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("lstat root %q: %w", root, err)
	}
	if w.opts.OneFileSystem && !w.devSet {
		if stat, ok := st.Sys().(*syscall.Stat_t); ok {
			w.devSeen = uint64(stat.Dev)
			w.devSet = true
		}
	}
	if st.IsDir() {
		// Strict mode: fail before emitting anything if the root cannot be
		// read at all, so an unreadable tree never produces a partial tar.
		if w.opts.Strict {
			if _, err := os.ReadDir(root); err != nil {
				return w.handleWalkError(fmt.Errorf("readdir root %q: %w", root, err), root)
			}
		}
		if err := w.emitOne(ctx, base, root, st); err != nil {
			return err
		}
		return w.walkDir(ctx, base, root)
	}
	return w.emitOne(ctx, base, root, st)
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

func (w *tarWriter) walkDir(ctx context.Context, arcRoot, fsRoot string) error {
	type item struct{ rel, full string }
	entries, err := os.ReadDir(fsRoot)
	if err != nil {
		return w.handleWalkError(fmt.Errorf("readdir %q: %w", fsRoot, err), fsRoot)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	stack := make([]item, 0, len(entries))
	for _, de := range entries {
		stack = append(stack, item{arcRoot + "/" + de.Name(), filepath.Join(fsRoot, de.Name())})
	}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("archiving %q: %w", fsRoot, err)
		}
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		rel := it.rel
		st, err := os.Lstat(it.full)
		if err != nil {
			if err := w.handleWalkError(fmt.Errorf("lstat %q: %w", it.full, err), it.full); err != nil {
				return err
			}
			continue
		}
		if st.IsDir() {
			sub, err := os.ReadDir(it.full)
			if err != nil {
				if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
					// Unreadable directory (e.g. 0500): archive the dir itself
					// and skip its contents; not an abort-worthy error.
					if err2 := w.emitOne(ctx, rel, it.full, st); err2 != nil {
						return err2
					}
					w.stats.Skipped++
					continue
				}
				if err2 := w.handleWalkError(fmt.Errorf("readdir %q: %w", it.full, err), it.full); err2 != nil {
					return err2
				}
				continue
			}
			// Emit the dir first, then children (deterministic order: dir before content).
			if err := w.emitOne(ctx, rel, it.full, st); err != nil {
				return err
			}
			names := make([]string, 0, len(sub))
			for _, de := range sub {
				names = append(names, de.Name())
			}
			sort.Strings(names)
			for i := len(names) - 1; i >= 0; i-- {
				stack = append(stack, item{rel + "/" + names[i], filepath.Join(it.full, names[i])})
			}
			continue
		}
		if err := w.emitOne(ctx, rel, it.full, st); err != nil {
			return err
		}
	}
	return nil
}

func (w *tarWriter) emitOne(ctx context.Context, arcPath, fsPath string, st os.FileInfo) error {
	if w.opts.OneFileSystem && w.devSet {
		if stat, ok := st.Sys().(*syscall.Stat_t); ok && uint64(stat.Dev) != w.devSeen {
			w.stats.Skipped++
			return nil
		}
	}
	e := &Entry{Path: arcPath}
	mode := st.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		e.Type = TypeSymlink
		tgt, err := os.Readlink(fsPath)
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
	if err := readMeta(fsPath, st, w.opts, e); err != nil {
		return w.handleWalkError(fmt.Errorf("metadata %q: %w", fsPath, err), fsPath)
	}
	if w.excluded(e.Path) {
		w.stats.Skipped++
		return nil
	}
	return w.writeEntry(ctx, e, fsPath, st)
}

func (w *tarWriter) excluded(arcPath string) bool {
	for _, pat := range w.opts.Excludes {
		if ok, err := filepath.Match(pat, arcPath); err == nil && ok {
			return true
		}
		trimmed := strings.TrimSuffix(pat, "/")
		if arcPath == trimmed || strings.HasPrefix(arcPath, trimmed+"/") {
			return true
		}
	}
	return false
}

func (w *tarWriter) handleWalkError(err error, _ string) error {
	if w.opts.Strict {
		return err
	}
	w.stats.Errors = append(w.stats.Errors, err)
	w.stats.Skipped++
	return nil
}

func (w *tarWriter) writeEntry(ctx context.Context, e *Entry, fsPath string, st os.FileInfo) error {
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
	if e.Type == TypeRegular && st.Sys() != nil {
		if stt, ok := st.Sys().(*syscall.Stat_t); ok && stt.Nlink > 1 {
			key := hardlinkKey{Dev: uint64(stt.Dev), Ino: stt.Ino}
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
	var f *os.File
	if e.Type == TypeRegular {
		var err error
		f, err = os.Open(fsPath)
		if err != nil {
			if w.opts.Strict {
				return w.handleWalkError(fmt.Errorf("open %q: %w", fsPath, err), fsPath)
			}
			// Degraded mode: skip the payload, keep the metadata entry as an
			// empty regular file (a header without matching size would be a
			// corrupt tar). Content mismatch is reported to the caller.
			w.stats.Errors = append(w.stats.Errors, fmt.Errorf("open %q: %w", fsPath, err))
			e.Size = 0
			hdr.Size = 0
		} else {
			defer f.Close()
		}
	}
	// archive/tar may defer the padding of the previous regular file until
	// WriteHeader. Record the logical start of the next header, not the number
	// of bytes which happened to reach the underlying writer so far.
	e.TarOffset = (w.written + 511) &^ int64(511)
	if err := w.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %q: %w", e.Path, err)
	}
	if e.Type == TypeRegular && f != nil {
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(w.tw, h), &ctxReader{ctx: ctx, r: f})
		if err != nil {
			return fmt.Errorf("copy %q: %w", fsPath, err)
		}
		e.SHA256 = hex.EncodeToString(h.Sum(nil))
		if err != nil {
			return fmt.Errorf("copy %q: %w", fsPath, err)
		}
		if n != e.Size {
			msg := fmt.Errorf("size changed while archiving %q: stat %d, read %d", fsPath, e.Size, n)
			if w.opts.Strict {
				return msg
			}
			w.stats.Errors = append(w.stats.Errors, msg)
		}
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

func (c *ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	return c.r.Read(p)
}
