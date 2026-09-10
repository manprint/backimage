package archive

import (
	"fmt"
	"strings"

	"github.com/manprint/backimage/internal/pathglob"
)

// The selection rules live here, outside the per-platform extractors, because
// which entries a restore is supposed to write is not a property of the
// operating system. The Windows extractor used to have none of this: it
// ignored --include, --exclude and --strip-components entirely, so a selective
// restore silently produced the whole backup.

// selects reports whether name passes the include/exclude filters.
//
// A pattern ending in "/" also selects everything below it, which is what a
// user means by `--include 'var/log/'`.
func selects(opts ExtractOptions, name string) bool {
	if len(opts.Includes) > 0 {
		ok := false
		for _, pat := range opts.Includes {
			if pathglob.Match(pat, name) {
				ok = true
				break
			}
			if strings.HasSuffix(pat, "/") && strings.HasPrefix(name, strings.TrimSuffix(pat, "/")+"/") {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, pat := range opts.Excludes {
		if pathglob.Match(pat, name) {
			return false
		}
		if strings.HasSuffix(pat, "/") && strings.HasPrefix(name, strings.TrimSuffix(pat, "/")+"/") {
			return false
		}
	}
	return true
}

// stripComponents drops count leading path components, reporting false when
// the name has no component left.
func stripComponents(name string, count int) (string, bool) {
	if count <= 0 {
		return name, name != ""
	}
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) <= count {
		return "", false
	}
	return strings.Join(parts[count:], "/"), true
}

// checkArchivePath refuses the names that must never reach the filesystem.
//
// It is no longer the traversal defence: os.Root refuses anything that leaves
// the destination, whatever the tree does while the restore runs. What is left
// here is the archive's own hygiene — an absolute name, a "..", an empty name
// — reported once as a clear error instead of as a syscall failure somewhere
// in the middle of the walk.
func checkArchivePath(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") ||
		strings.Contains(name, "/../") || strings.HasSuffix(name, "/..") {
		return fmt.Errorf("unsafe archive path %q", name)
	}
	return nil
}
