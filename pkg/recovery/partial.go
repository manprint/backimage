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
		buffered, err := b.readRange(e.TarOffset, end, load)
		if err != nil {
			report.Skipped++
			if len(report.SkippedPaths) < skippedPathsCap {
				report.SkippedPaths = append(report.SkippedPaths, e.Path)
			}
			continue
		}
		if _, err := dst.Write(buffered); err != nil {
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

// readRange collects [start,end) from the chunks that cover it. It buffers the
// range instead of writing it out directly: an entry must be written whole or
// not at all, or a chunk failing halfway would leave a truncated record in the
// tar and break every entry after it.
func (b *Backup) readRange(start, end int64, load func(int) ([]byte, error)) ([]byte, error) {
	out := make([]byte, 0, end-start)
	for off := start; off < end; {
		i := sort.Search(len(b.Chunks.Chunks), func(i int) bool { return b.prefix[i+1] > off })
		if i >= len(b.Chunks.Chunks) {
			return nil, fmt.Errorf("offset tar %d fuori dalla tabella dei chunk", off)
		}
		data, err := load(i)
		if err != nil {
			return nil, err
		}
		within := off - b.prefix[i]
		n := int64(len(data)) - within
		if remaining := end - off; n > remaining {
			n = remaining
		}
		if n <= 0 {
			return nil, fmt.Errorf("il chunk %d non copre l'offset tar %d", i, off)
		}
		out = append(out, data[within:within+n]...)
		off += n
	}
	return out, nil
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
