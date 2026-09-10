package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/manprint/backimage/pkg/index"
)

// PartialReport describes what a partial recovery could and could not write.
type PartialReport struct {
	Entries      int      `json:"entries"`                // entries written to the tar
	Skipped      int      `json:"skipped"`                // entries dropped
	BadChunks    []int    `json:"badChunks,omitempty"`    // chunks that failed to load or verify
	SkippedPaths []string `json:"skippedPaths,omitempty"` // capped sample of the dropped paths
	Causes       []string `json:"causes,omitempty"`       // one message per bad chunk
}

// skippedPathsCap keeps a failure report readable when a whole layer is gone.
const skippedPathsCap = 50

// StreamTarPartial writes a valid tar of every entry whose bytes lie entirely
// in chunks that load and verify, and reports the entries it had to drop.
//
// It exists because a single damaged chunk used to cost the whole restore: the
// stream is sequential, so an integrity error at chunk 393 of 520 also lost
// the 127 intact chunks after it. Working from the index instead of the raw
// stream turns that into a bounded loss — the files that live in the damaged
// chunk — and names them, which is what makes the remaining data trustworthy.
//
// A chunk is loaded at most once: entries are visited in tar order, and a
// chunk that failed is remembered so the following entries are dropped without
// retrying it.
func (b *Backup) StreamTarPartial(ctx context.Context, idx *index.Index, dst io.Writer, verify bool) (PartialReport, error) {
	return b.streamTarPartial(ctx, idx, nil, dst, verify)
}

// StreamSelectedTarPartial is StreamTarPartial restricted to selected: it
// tolerates damaged chunks *and* honours the filters, which used to be an
// either-or.
//
// --continue swapped the selective stream for the tolerant one, and the
// tolerant one had no notion of a selection: `--continue --include '**/*.pdf'`
// therefore wrote the entire backup, and with --overwrite wrote it over the
// destination. Combining the two here is what removes the choice between
// "salvage what survives" and "restore only what I asked for".
//
// The entry ranges stay per-entry, as in the unfiltered case: merging
// neighbouring ranges the way the non-partial selective stream does would make
// one damaged chunk drop every entry sharing a range with it.
func (b *Backup) StreamSelectedTarPartial(ctx context.Context, idx *index.Index, selected []index.FileEntry, dst io.Writer, verify bool) (PartialReport, error) {
	if idx == nil {
		return PartialReport{}, errors.New("il recupero parziale richiede l'indice dei file")
	}
	return b.streamTarPartial(ctx, idx, selectionSet(idx, selected), dst, verify)
}

// streamTarPartial carries both variants. wanted nil means every entry.
func (b *Backup) streamTarPartial(ctx context.Context, idx *index.Index, wanted map[string]bool, dst io.Writer, verify bool) (PartialReport, error) {
	report := PartialReport{}
	if idx == nil {
		return report, errors.New("il recupero parziale richiede l'indice dei file")
	}
	verify = b.mustVerify(verify)
	total := b.prefix[len(b.prefix)-1]
	contentEnd := total
	if contentEnd >= 1024 {
		contentEnd -= 1024 // the original trailer is replaced below
	}

	cacheIndex := -1
	used := 0
	var cache []byte
	defer clear(cache)
	bad := make(map[int]bool)
	load := func(chunkIndex int) ([]byte, error) {
		if chunkIndex == cacheIndex {
			return cache, nil
		}
		if bad[chunkIndex] {
			return nil, fmt.Errorf("chunk %d già segnalato come danneggiato", chunkIndex)
		}
		clear(cache)
		cacheIndex = -1
		used++
		data, err := b.plainChunkBytes(ctx, chunkIndex, verify)
		if err != nil {
			bad[chunkIndex] = true
			report.Causes = append(report.Causes, err.Error())
			return nil, err
		}
		cacheIndex, cache = chunkIndex, data
		return cache, nil
	}

	// One range per entry: merging them, as a selective restore does, would
	// make one damaged chunk drop every neighbour in the same run.
	//
	// The range of an entry is computed from the *full* index even when a
	// selection is active: where an entry ends is where the next one begins,
	// and that neighbour is a property of the archive, not of what was asked
	// for.
	for i, e := range idx.Entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		end := contentEnd
		if i+1 < len(idx.Entries) {
			end = idx.Entries[i+1].TarOffset
		}
		if e.TarOffset < 0 || end <= e.TarOffset || end > contentEnd {
			return report, fmt.Errorf("%w: offset tar non validi per %q", index.ErrBadSchema, e.Path)
		}
		if wanted != nil && !wanted[e.Path] {
			continue
		}
		// Pass 1 proves that every chunk covering the entry loads, before a
		// byte of it is written. The chunks are already verified by
		// plainChunkBytes, so this pass is not a second verification: it buys
		// the guarantee the per-entry buffer used to buy — an entry is written
		// whole or not at all — without holding the entry in memory.
		if err := b.walkRange(e.TarOffset, end, load, nil); err != nil {
			report.Skipped++
			if len(report.SkippedPaths) < skippedPathsCap {
				report.SkippedPaths = append(report.SkippedPaths, e.Path)
			}
			continue
		}
		// Pass 2 walks the same range into the destination. A failure here is
		// fatal rather than a skip: bytes of this entry are already in the
		// tar, and continuing would leave a truncated record that breaks every
		// entry after it.
		if err := b.walkRange(e.TarOffset, end, load, func(p []byte) error {
			_, werr := dst.Write(p)
			return werr
		}); err != nil {
			return report, err
		}
		report.Entries++
	}
	if _, err := dst.Write(make([]byte, 1024)); err != nil {
		return report, err
	}
	for i := range bad {
		report.BadChunks = append(report.BadChunks, i)
	}
	sort.Ints(report.BadChunks)
	b.reportIntegrity(used-len(bad), len(b.Chunks.Chunks), verify)
	return report, nil
}

// walkRange visits [start,end) chunk by chunk and hands each covering slice to
// fn, which may be nil to walk without consuming anything.
//
// It replaces a readRange that collected the whole range into one buffer sized
// end-start. That buffer was not accidental — an entry must be written whole
// or not at all — but it made the resident memory of a partial recovery equal
// to the largest file in the backup: a 50 GB file inside the archive meant 50
// GB of process memory. Walking the range twice, once to prove every chunk
// loads and once to write, keeps the same guarantee at the cost of one chunk
// of memory.
//
// Cost of the second walk: the one-chunk cache makes it free for an entry
// contained in a single chunk, which is the common case; an entry spanning
// several chunks pays for decrypting and decompressing each of them twice.
func (b *Backup) walkRange(start, end int64, load func(int) ([]byte, error), fn func([]byte) error) error {
	for off := start; off < end; {
		i := sort.Search(len(b.Chunks.Chunks), func(i int) bool { return b.prefix[i+1] > off })
		if i >= len(b.Chunks.Chunks) {
			return fmt.Errorf("offset tar %d fuori dalla tabella dei chunk", off)
		}
		data, err := load(i)
		if err != nil {
			return err
		}
		within := off - b.prefix[i]
		n := int64(len(data)) - within
		if remaining := end - off; n > remaining {
			n = remaining
		}
		if n <= 0 {
			return fmt.Errorf("il chunk %d non copre l'offset tar %d", i, off)
		}
		if fn != nil {
			if err := fn(data[within : within+n]); err != nil {
				return err
			}
		}
		off += n
	}
	return nil
}

// Summary renders the audit evidence of a partial recovery.
func (r PartialReport) Summary() []string {
	if r.Skipped == 0 {
		return []string{fmt.Sprintf(
			"recupero parziale non necessario: %d entry ricostruite, nessun chunk danneggiato", r.Entries)}
	}
	lines := []string{fmt.Sprintf(
		"ATTENZIONE: recupero parziale: %d entry ricostruite, %d NON recuperabili perché ricadono nei chunk danneggiati %v",
		r.Entries, r.Skipped, r.BadChunks)}
	for _, cause := range r.Causes {
		lines = append(lines, "  causa: "+cause)
	}
	shown := r.SkippedPaths
	if len(shown) > 0 {
		lines = append(lines, "  percorsi perduti: "+strings.Join(shown, ", "))
	}
	if r.Skipped > len(shown) {
		lines = append(lines, fmt.Sprintf("  ... e altri %d percorsi non elencati", r.Skipped-len(shown)))
	}
	return lines
}
