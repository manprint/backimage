//go:build unix || windows

package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Totals counts what a walk of the roots would archive.
type Totals struct {
	Files int64 // regular files
	Bytes int64 // their size, as lstat reports it
}

// Estimate walks the roots the way a Writer with the same options would,
// without reading any content, and counts the regular files it would
// archive. It honours Excludes and OneFileSystem exactly as the archiver
// does: an estimate that walked past a mount point the archive stops at —
// /proc, a stale NFS mount — could take forever, and one that counted
// excluded bytes planned layers and a temp-space check for data that is never
// written.
//
// Errors follow opts.Strict: in strict mode the first one is returned, as the
// archiver would; otherwise the entry is left out of the count. A directory
// that cannot be listed for lack of permission is skipped in both modes,
// because the archiver skips its content in both modes.
func Estimate(ctx context.Context, roots []string, opts Options) (Totals, error) {
	e := estimator{ctx: ctx, opts: opts}
	for _, root := range roots {
		if err := e.root(filepath.Clean(root)); err != nil {
			return e.totals, err
		}
	}
	return e.totals, nil
}

type estimator struct {
	ctx     context.Context
	opts    Options
	totals  Totals
	devSeen uint64
	devSet  bool
}

func (e *estimator) root(root string) error {
	base := filepath.Base(root)
	src := source{name: base, path: root}
	st, err := src.lstat()
	if err != nil {
		return fmt.Errorf("lstat root %q: %w", root, err)
	}
	if e.opts.OneFileSystem && !e.devSet {
		if dev, ok := deviceOf(st); ok {
			e.devSeen = dev
			e.devSet = true
		}
	}
	if e.otherDevice(st) {
		return nil
	}
	if !st.IsDir() {
		e.count(base, st)
		return nil
	}
	l, err := src.list(st)
	if err != nil {
		return e.fail(fmt.Errorf("readdir root %q: %w", root, err))
	}
	defer l.close()
	l.releaseFile()
	return e.children(base, root, l)
}

func (e *estimator) children(arcDir, dirPath string, l *listing) error {
	for _, name := range l.names {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		src := source{dir: l.dir, name: name, path: filepath.Join(dirPath, name)}
		if err := e.entry(arcDir+"/"+name, src); err != nil {
			return err
		}
	}
	return nil
}

func (e *estimator) entry(arcPath string, src source) error {
	st, err := src.lstat()
	if err != nil {
		return e.fail(fmt.Errorf("lstat %q: %w", src.path, err))
	}
	if e.otherDevice(st) {
		return nil
	}
	if !st.IsDir() {
		e.count(arcPath, st)
		return nil
	}
	l, err := src.list(st)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil
		}
		return e.fail(fmt.Errorf("readdir %q: %w", src.path, err))
	}
	defer l.close()
	l.releaseFile()
	return e.children(arcPath, src.path, l)
}

func (e *estimator) otherDevice(st os.FileInfo) bool {
	if !e.opts.OneFileSystem || !e.devSet {
		return false
	}
	dev, ok := deviceOf(st)
	return ok && dev != e.devSeen
}

func (e *estimator) count(arcPath string, st os.FileInfo) {
	if st.Mode().IsRegular() && !excludedBy(e.opts.Excludes, arcPath) {
		e.totals.Files++
		e.totals.Bytes += st.Size()
	}
}

func (e *estimator) fail(err error) error {
	if e.opts.Strict {
		return err
	}
	return nil
}
