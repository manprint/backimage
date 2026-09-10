//go:build windows

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
	"time"
)

// The Windows extractor.
//
// It used to be ninety lines that ignored --include, --exclude and
// --strip-components outright — a selective restore produced the whole backup,
// silently, on every Windows host — and turned every entry it did not know
// about into an empty regular file, so a hardlink, a device and a fifo all
// arrived as zero bytes with no error. It also had no confinement: names were
// joined to the destination and opened, with nothing stopping a pre-existing
// symlink or junction from redirecting the write.
//
// This rewrite shares the selection rules with the Unix path (selection.go),
// anchors every operation to an os.Root, and reports what Windows cannot hold
// instead of inventing a file. What it still cannot do is documented in
// docs/FIDELITY.md: POSIX ownership, modes beyond the read-only bit, extended
// attributes, devices and fifos.
type windowsExtractor struct {
	opts     ExtractOptions
	dest     string
	warnings []string
	warned   map[string]bool
	degraded map[string]int64
	examples map[string]string
}

func extractorFor(opts ExtractOptions) Extractor {
	return &windowsExtractor{
		opts:     opts,
		warned:   make(map[string]bool),
		degraded: make(map[string]int64),
		examples: make(map[string]string),
	}
}

func (x *windowsExtractor) show(name string) string {
	return filepath.Join(x.dest, filepath.FromSlash(name))
}

func (x *windowsExtractor) note(class string, err error) {
	x.degraded[class]++
	if err != nil && x.examples[class] == "" {
		x.examples[class] = err.Error()
	}
}

func (x *windowsExtractor) warn(key, message string) {
	if x.warned[key] {
		return
	}
	x.warned[key] = true
	x.warnings = append(x.warnings, message)
	if x.opts.Progress != nil {
		x.opts.Progress("restore: attenzione: " + message)
	}
}

func (x *windowsExtractor) degrade(class, key, message string, err error) error {
	if x.opts.Strict {
		return err
	}
	x.note(class, err)
	x.warn(key, message)
	return nil
}

// reservedChars are the bytes a Windows filename cannot contain. A Unix name
// holding one of them is not restorable here under its real name, and
// rewriting it would produce a file the backup does not describe: the entry is
// reported as skipped instead.
const reservedChars = `\:*?"<>|`

func windowsUsable(name string) error {
	if strings.ContainsAny(name, reservedChars) {
		return fmt.Errorf("%q: il nome contiene caratteri che Windows non ammette (%s): %w",
			name, reservedChars, errUnsupportedName)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") {
			return fmt.Errorf("%q: Windows non conserva spazi o punti finali nei nomi: %w",
				name, errUnsupportedName)
		}
	}
	return nil
}

var (
	errNeedOverwrite    = errors.New("destinazione già esistente: usa --overwrite")
	errUnsupportedEntry = errors.New("tipo di entry non supportato su Windows")
	errUnsupportedName  = errors.New("nome non rappresentabile su Windows")
	// errLinkTargetNotRestored mirrors the Unix rule: a hardlink may only
	// point at a regular file this same run has written.
	errLinkTargetNotRestored = errors.New(
		"il primo nome dell'hardlink non è stato ripristinato in questa corsa: entry saltata")
)

func (x *windowsExtractor) Extract(ctx context.Context, r io.Reader, dest string) (stats Stats, err error) {
	defer func() { stats.Warnings = x.warnings }()
	tr := tar.NewReader(r)
	dest = filepath.Clean(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return stats, fmt.Errorf("mkdir dest %q: %w", dest, err)
	}
	x.dest = dest
	// os.Root on Windows also refuses to walk through a junction or a symlink
	// that leaves the destination, which is what this path had no defence
	// against at all.
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
		mode fs.FileMode
		mt   time.Time
	}
	var dirFixes []dirFix
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
			if x.opts.Strict {
				return stats, err
			}
			continue
		}
		if err := x.createOne(root, name, hdr, tr, materialised, &stats); err != nil {
			if x.opts.Strict || errors.Is(err, errNeedOverwrite) || errors.Is(err, io.ErrUnexpectedEOF) {
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
			dirFixes = append(dirFixes, dirFix{name: name, mode: headerMode(hdr), mt: hdr.ModTime})
		}
	}

	if x.opts.Progress != nil {
		x.opts.Progress("restore: filesystem: finalizzazione metadati directory")
	}
	sort.Slice(dirFixes, func(i, j int) bool { return len(dirFixes[i].name) > len(dirFixes[j].name) })
	for _, d := range dirFixes {
		if err := root.Chmod(d.name, d.mode); err != nil {
			if err := x.degrade("mode", "mode-dir", modeDegradeMsg,
				fmt.Errorf("chmod dir %q: %w", x.show(d.name), err)); err != nil {
				return stats, err
			}
		}
		if !d.mt.IsZero() {
			if err := root.Chtimes(d.name, time.Time{}, d.mt); err != nil {
				if err := x.degrade("times", "times-dir", timesDegradeMsg,
					fmt.Errorf("chtimes dir %q: %w", x.show(d.name), err)); err != nil {
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

func (x *windowsExtractor) createOne(
	root *os.Root,
	name string,
	hdr *tar.Header,
	tr *tar.Reader,
	materialised map[string]bool,
	stats *Stats,
) error {
	shown := x.show(name)
	if err := windowsUsable(name); err != nil {
		x.warn("unsupported-name", "alcuni nomi del backup non sono rappresentabili su Windows: "+
			"le relative entry sono state saltate (elenco in Stats.Errors / --json)")
		return err
	}

	// Same overwrite semantics as the Unix path: the archive is overlaid on
	// the destination, and only an object whose type differs is removed first.
	if existing, err := root.Lstat(name); err == nil {
		if !x.opts.Overwrite {
			return fmt.Errorf("%q già esistente: %w", shown, errNeedOverwrite)
		}
		if !(existing.IsDir() && hdr.Typeflag == tar.TypeDir) {
			if err := root.RemoveAll(name); err != nil {
				return fmt.Errorf("remove existing %q: %w", shown, err)
			}
			delete(materialised, name)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %q: %w", shown, err)
	}

	if parent := path.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("mkdir parent %q: %w", shown, err)
		}
	}

	mode := headerMode(hdr)
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(name, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", shown, err)
		}
		stats.Dirs++
	case tar.TypeReg:
		f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
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
			// Creating a symlink needs SeCreateSymbolicLinkPrivilege or
			// developer mode. Without it the entry is lost, and saying so is
			// the whole point: an empty file in its place is worse.
			return fmt.Errorf("symlink %q: %w (Windows richiede la modalità sviluppatore "+
				"o il privilegio SeCreateSymbolicLinkPrivilege)", shown, err)
		}
		stats.Symlinks++
	case tar.TypeLink:
		if !materialised[hdr.Linkname] {
			return fmt.Errorf("hardlink %q -> %q: %w", shown, hdr.Linkname, errLinkTargetNotRestored)
		}
		if err := root.Link(hdr.Linkname, name); err != nil {
			copied, cerr := copyFileIn(root, hdr.Linkname, name, mode)
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
		materialised[name] = true
	default:
		// Devices, fifos, sockets and anything else: Windows has no
		// equivalent. They are reported, never materialised as empty files.
		x.warn("unsupported-type", "il backup contiene device, fifo o altri oggetti POSIX "+
			"che Windows non può ricreare: le relative entry sono state saltate")
		return fmt.Errorf("%q: typeflag %q: %w", hdr.Name, hdr.Typeflag, errUnsupportedEntry)
	}

	if x.opts.PreserveOwner {
		x.warn("owner-windows", "owner e gruppo POSIX non sono ripristinabili su Windows: ignorati")
	}
	if x.opts.PreserveXattrs && len(hdr.PAXRecords) > 0 {
		x.warn("xattr-windows", "gli extended attribute non sono ripristinabili su Windows: ignorati")
		for k := range hdr.PAXRecords {
			if strings.HasPrefix(k, "SCHILY.xattr.") {
				stats.XattrsSkipped++
			}
		}
	}
	if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeSymlink {
		if err := root.Chmod(name, mode.Perm()); err != nil {
			if err := x.degrade("mode", "mode", modeDegradeMsg,
				fmt.Errorf("chmod %q: %w", shown, err)); err != nil {
				return err
			}
		}
		if !hdr.ModTime.IsZero() {
			if err := root.Chtimes(name, time.Time{}, hdr.ModTime); err != nil {
				if err := x.degrade("times", "times", timesDegradeMsg,
					fmt.Errorf("chtimes %q: %w", shown, err)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// copyFileIn duplicates src into dst, both resolved through root. It is the
// hardlink fallback, and src is always a name this run has already restored.
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

// headerMode reconstructs an os.FileMode from a tar header. Windows honours
// only the write bit, but the value is kept whole so the reporting is honest.
func headerMode(hdr *tar.Header) fs.FileMode {
	return fs.FileMode(hdr.Mode & 0o7777) // #nosec G115 -- masked to 12 bits
}

const (
	modeDegradeMsg = "permessi non applicabili su alcune entry: " +
		"resta il mode di creazione (contenuti invariati)"
	timesDegradeMsg = "timestamp non applicabili su alcune entry: " +
		"resta l'ora di estrazione (contenuti invariati)"
	hardlinkDegradeMsg = "hardlink non ricreabili su questa destinazione: " +
		"materializzati come copie indipendenti (nessun byte perso, spazio su disco maggiore)"
)
