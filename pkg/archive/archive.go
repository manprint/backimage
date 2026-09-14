package archive

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// Options controls archiving behaviour.
type Options struct {
	Strict         bool     // any read error aborts the operation (default true)
	FollowSymlink  bool     // default false: symlinks are archived, not followed
	OneFileSystem  bool     // do not cross mount points
	Excludes       []string // glob patterns matched against Entry.Path
	NumericOwner   bool     // do not resolve Uname/Gname
	PreserveACLs   bool     // default true
	PreserveXattrs bool     // default true
	PreserveTimes  bool     // archive AccessTime/ChangeTime as PAX records (default false; keeps archives deterministic)
}

// Stats accumulates what happened during a walk.
type Stats struct {
	Files, Dirs, Symlinks, Hardlinks, Devices, Fifos, Skipped int64
	BytesRaw                                                  int64
	ContentSkipped                                            int64             // backup only: regular files archived without their content (unreadable, degraded mode)
	XattrsSkipped                                             int64             // extended attributes dropped during a restore
	Degraded                                                  map[string]int64  // restore only: operations dropped, by class ("owner", "mode", "times", "xattr.trusted", "hardlink", "object")
	DegradedExamples                                          map[string]string // one real failure per degraded class, as evidence
	Errors                                                    []error           // populated only when Strict is false
	Warnings                                                  []string          // non-fatal degradations, one line per distinct cause
}

// FidelityLines renders the audit evidence of a restore: either the single
// line that states the extraction was 1:1, or the verdict followed by one line
// per difference, with its count and a real example. The caller logs them
// verbatim; this is the only place that decides what "1:1" means.
func (s Stats) FidelityLines() []string {
	objects := s.Files + s.Dirs + s.Symlinks + s.Hardlinks + s.Devices + s.Fifos
	if len(s.Degraded) == 0 && s.Skipped == 0 {
		// Scoped to what the extractor received: a partial recovery upstream
		// may have dropped entries before they ever reached this stream, and
		// only its own report can account for those.
		return []string{fmt.Sprintf(
			"esito 1:1 sulle entry ricevute: %d oggetti ripristinati (%d file, %d directory, %d symlink, %d hardlink, %d device, %d fifo); "+
				"contenuti, permessi, owner, timestamp e attributi estesi applicati integralmente; nessuna differenza",
			objects, s.Files, s.Dirs, s.Symlinks, s.Hardlinks, s.Devices, s.Fifos)}
	}
	lines := []string{fmt.Sprintf(
		"esito NON 1:1 sulle entry ricevute: %d oggetti ripristinati, %d entry non create, %d differenze di metadati per classe:",
		objects, s.Skipped, totalDegraded(s.Degraded))}
	classes := make([]string, 0, len(s.Degraded))
	for class := range s.Degraded {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		line := fmt.Sprintf("  differenza %s: %d", class, s.Degraded[class])
		if example := s.DegradedExamples[class]; example != "" {
			line += " (es. " + example + ")"
		}
		lines = append(lines, line)
	}
	if s.Skipped > 0 {
		lines = append(lines, fmt.Sprintf(
			"  %d entry NON estratte: elenco completo in Stats.Errors (--json)", s.Skipped))
	}
	return lines
}

// ClosingVerdict renders the one line a restore ends with, in both binaries:
// what was produced, whether every chunk was authenticated, and whether the
// tree is a faithful copy of what the backup holds.
//
// FidelityLines explains the differences; this states the outcome. It exists
// because the two facts a restore has to leave behind — "the bytes are the
// bytes" and "the tree is the tree" — were reported by two subsystems several
// lines apart, and the last line of a successful run said only how long it
// took. This is the line to grep for in a log or a cron mail.
func (s Stats) ClosingVerdict(chunksVerified bool) string {
	objects := s.Files + s.Dirs + s.Symlinks + s.Hardlinks + s.Devices + s.Fifos
	integrity := "tutti i chunk verificati (digest in chiaro coincidenti con quelli registrati nel backup)"
	if !chunksVerified {
		integrity = "digest dei chunk NON verificati (--no-verify)"
	}
	differences := totalDegraded(s.Degraded)
	if differences == 0 && s.Skipped == 0 {
		return fmt.Sprintf(
			"ESITO: estrazione 1:1, nessun errore — %d oggetti ripristinati, "+
				"0 differenze di metadati, 0 entry saltate; %s",
			objects, integrity)
	}
	return fmt.Sprintf(
		"ESITO: estrazione NON 1:1 — %d oggetti ripristinati, %d differenze di metadati, "+
			"%d entry non create; %s. Il dettaglio è nelle righe \"differenza\" qui sopra",
		objects, differences, s.Skipped, integrity)
}

func totalDegraded(degraded map[string]int64) int64 {
	var total int64
	for _, n := range degraded {
		total += n
	}
	return total
}

// Writer streams a deterministic PAX tar of the given roots.
type Writer interface {
	// AddRoot archives one root path. Roots are processed in the order given.
	AddRoot(ctx context.Context, root string) error
	// Close flushes the tar trailer and returns the accumulated statistics.
	Close() (Stats, error)
	// Entries returns the entries emitted so far, in emission order.
	Entries() []Entry
}

// NewWriter builds a Writer that writes to w.
func NewWriter(w io.Writer, opts Options) Writer { return newWriter(w, opts) }

// Reader reads a PAX tar produced by this package.
type Reader interface {
	// Next advances to the next entry. Returns io.EOF at the end.
	Next() (*Entry, io.Reader, error)
}

// NewReader builds a Reader over r.
func NewReader(r io.Reader) Reader { return newReader(r) }

// Extractor materialises entries onto the filesystem.
type Extractor interface {
	// Extract consumes a tar stream and writes it under dest.
	Extract(ctx context.Context, r io.Reader, dest string) (Stats, error)
}

// NewExtractor returns the platform extractor.
func NewExtractor(opts ExtractOptions) Extractor { return extractorFor(opts) }

// ExtractOptions controls restore behaviour.
type ExtractOptions struct {
	PreserveOwner   bool     // default true; requires privileges
	PreserveXattrs  bool     // default true
	Overwrite       bool     // default false: existing files cause an error
	Includes        []string // if non-empty, only matching paths are extracted
	Excludes        []string
	StripComponents int          // remove this many leading path components
	Strict          bool         // default true; false tolerates metadata that cannot be applied
	Progress        func(string) // optional diagnostics for filesystem phases
}
