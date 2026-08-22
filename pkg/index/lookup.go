package index

import (
	"fmt"
	"path"
	"sort"

	"github.com/manprint/backimage/internal/pathglob"
)

// tarHeaderBlock is the size of a tar header record including padding.
const tarHeaderBlock = 512

// Locator answers "which chunks do I need" questions over a chunk table.
// The plaintext stream is the concatenation of chunk plaintexts in index
// order: chunk i covers [prefix[i], prefix[i+1]) in the stream.
type Locator struct {
	t      *ChunkTable
	prefix []int64 // prefix[i] = plain bytes before chunk i; len = n+1
	total  int64
}

// NewLocator builds a locator from a validated chunk table (ReadChunkTable
// contracts). Negative sizes in the table produce a locator whose searches
// return usage errors rather than panicking.
//
// It relies on the plain sizes, which are confidential in an encrypted backup:
// pass a table whose Pb fields have been filled by MergePrivate, otherwise
// every chunk looks empty.
func NewLocator(t *ChunkTable) *Locator {
	if t == nil {
		return &Locator{t: &ChunkTable{}}
	}
	l := &Locator{t: t, prefix: make([]int64, len(t.Chunks)+1)}
	for i, c := range t.Chunks {
		if c.Pb < 0 {
			return &Locator{t: t}
		}
		l.prefix[i+1] = l.prefix[i] + c.Pb
	}
	l.total = l.prefix[len(t.Chunks)]
	return l
}

// ChunkForOffset returns the index of the chunk containing off in the
// plaintext tar stream, and the offset within that chunk.
func (l *Locator) ChunkForOffset(off int64) (int, int64, error) {
	if len(l.prefix) == 0 {
		return 0, 0, fmt.Errorf("empty chunk table")
	}
	if off < 0 || off >= l.total {
		return 0, 0, fmt.Errorf("offset %d out of range (stream is %d bytes)", off, l.total)
	}
	i := l.chunkAt(off)
	return i, off - l.prefix[i], nil
}

// Range returns the inclusive chunk index range covering [start,end) of the
// plaintext stream.
func (l *Locator) Range(start, end int64) (from, to int, err error) {
	if start < 0 || end <= start || end > l.total {
		return 0, 0, fmt.Errorf("range [%d,%d) invalid for %d-byte stream", start, end, l.total)
	}
	return l.chunkAt(start), l.chunkAt(end - 1), nil
}

// chunkAt returns the chunk index covering off (off must be in [0,total)).
func (l *Locator) chunkAt(off int64) int {
	// largest i with prefix[i] <= off
	i := sort.Search(len(l.prefix), func(i int) bool { return l.prefix[i] > off }) - 1
	if i < 0 {
		return 0
	}
	return i
}

// TotalPlainBytes returns the size of the plaintext tar stream.
func (l *Locator) TotalPlainBytes() int64 { return l.total }

// EntriesMatching filters index entries with include/exclude glob patterns.
// A pattern ending in "/" or matching a directory includes all its
// descendants; "**" matches any number of path segments.
func EntriesMatching(idx *Index, includes, excludes []string) ([]FileEntry, error) {
	if idx == nil {
		return nil, fmt.Errorf("nil index")
	}
	if err := pathglob.Validate(includes, excludes); err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		if len(includes) > 0 && !matchAnyEntry(e, includes) {
			continue
		}
		if matchAnyEntry(e, excludes) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func matchAnyEntry(e FileEntry, pats []string) bool {
	for _, p := range pats {
		if entryMatches(e, p) {
			return true
		}
	}
	return false
}

// entryMatches reports whether e matches pat. Beyond the plain glob the
// convenience rules apply: a pattern matching an ancestor directory (or a
// directory entry) includes the entry, and trailing "/" forces that.
func entryMatches(e FileEntry, pat string) bool {
	if pathglob.Match(pat, e.Path) {
		return true
	}
	dir := path.Dir(e.Path)
	for dir != "." && dir != "/" {
		if pathglob.Match(pat, dir) {
			return true
		}
		dir = path.Dir(dir)
	}
	return false
}

// entryRange returns the plaintext span [start,end) occupied by e: the tar
// header block (512 B) plus the payload rounded up to a 512-byte boundary.
func entryRange(e FileEntry) (start, end int64) {
	start = e.TarOffset
	data := (e.Size + tarHeaderBlock - 1) &^ (tarHeaderBlock - 1)
	return start, start + tarHeaderBlock + data
}

// ChunksFor returns the sorted, deduplicated set of chunk indices needed to
// extract the given entries.
func ChunksFor(l *Locator, entries []FileEntry) ([]int, error) {
	if l == nil {
		return nil, fmt.Errorf("nil locator")
	}
	seen := map[int]bool{}
	for _, e := range entries {
		start, end := entryRange(e)
		from, to, err := l.Range(start, end)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", e.Path, err)
		}
		for i := from; i <= to; i++ {
			seen[i] = true
		}
	}
	out := make([]int, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	sort.Ints(out)
	return out, nil
}
