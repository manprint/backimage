// Package pathglob matches slash-separated archive paths against globs.
//
// It exists so that every command that filters archived paths — `backup
// --exclude`, `restore --include/--exclude`, `ls`, `find` — agrees on what a
// pattern means. They used to disagree: the listing side understood a recursive
// "**" while `backup --exclude` went through filepath.Match, where "**" is just
// a single-segment wildcard. The result was an exclusion that quietly kept
// whatever sat two or more levels below the excluded directory, which on a
// backup tool means archiving data the operator asked to leave out.
package pathglob

import (
	"fmt"
	"path"
	"strings"
)

// Match reports whether the slash-separated name matches pat. Each pattern
// segment is matched with path.Match, so "*" and "?" stay inside one segment,
// while "**" spans any number of segments including none. A trailing "/" is
// ignored, so "dir/" and "dir" mean the same thing.
//
// Note that "dir/**" matches "dir" itself as well as everything under it: a
// pattern meant to drop a subtree drops the directory entry too, instead of
// leaving an empty directory behind.
func Match(pat, name string) bool {
	pat = strings.TrimSuffix(pat, "/")
	if pat == "" {
		return name == ""
	}
	return matchParts(strings.Split(pat, "/"), strings.Split(name, "/"))
}

func matchParts(p, n []string) bool {
	if len(p) == 0 {
		return len(n) == 0
	}
	if p[0] == "**" {
		for i := 0; i <= len(n); i++ {
			if matchParts(p[1:], n[i:]) {
				return true
			}
		}
		return false
	}
	if len(n) == 0 {
		return false
	}
	if ok, err := path.Match(p[0], n[0]); err != nil || !ok {
		return false
	}
	return matchParts(p[1:], n[1:])
}

// MatchAny reports whether name matches at least one pattern.
func MatchAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if Match(pat, name) {
			return true
		}
	}
	return false
}

// Validate rejects malformed patterns segment by segment. A filter is worth
// validating up front: path.Match answers "no match" to a pattern it could not
// parse, so a typo in an --exclude would otherwise read as "nothing to exclude"
// and archive the data anyway.
func Validate(groups ...[]string) error {
	for _, group := range groups {
		for _, pat := range group {
			for _, seg := range strings.Split(strings.TrimSuffix(pat, "/"), "/") {
				if seg == "**" {
					continue
				}
				if _, err := path.Match(seg, "x"); err != nil {
					return fmt.Errorf("bad glob %q: %w", pat, err)
				}
			}
		}
	}
	return nil
}
