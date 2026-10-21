package archive

import "strings"

// replacements records, in archive order, the names an extraction removed to
// make room for a later entry (--overwrite replacing a directory with a
// symlink, a file, a device).
//
// Anything recorded under such a name before its removal is stale: the
// directory fixes of the final pass and the hardlink targets this run wrote.
// Acting on them anyway resolves the old name through whatever replaced it —
// a symlink to a file or directory that was already in the destination — and
// changes an object this restore never wrote: a 0777 chmod on a pre-existing
// file, a hardlink to a pre-existing secret.
type replacements struct {
	step    int
	removed map[string]int // name -> step of its last removal
}

func newReplacements() *replacements {
	return &replacements{removed: make(map[string]int)}
}

// next starts the next archive entry and returns its step.
func (r *replacements) next() int {
	r.step++
	return r.step
}

// remove records that name, and so everything below it, was removed at the
// current step.
func (r *replacements) remove(name string) { r.removed[name] = r.step }

// since reports whether name or one of its ancestors was removed after step.
func (r *replacements) since(name string, step int) bool {
	for {
		if s, ok := r.removed[name]; ok && s > step {
			return true
		}
		i := strings.LastIndexByte(name, '/')
		if i < 0 {
			return false
		}
		name = name[:i]
	}
}
