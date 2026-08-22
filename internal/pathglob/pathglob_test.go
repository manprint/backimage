package pathglob

import "testing"

func TestMatchIsRecursiveOnDoubleStar(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		// The regression this package exists for: filepath.Match treated "**"
		// as a single-segment wildcard, so a file two levels down survived an
		// exclusion that named its ancestor.
		{"alice/.cache/**", "alice/.cache/cookies.db", true},
		{"alice/.cache/**", "alice/.cache/chromium/Default/Cookies", true},
		{"alice/.cache/**", "alice/.cache", true},
		{"alice/.cache/**", "alice/docs/cv.pdf", false},
		{"alice/.cache/**", "alice/.cacheable/x", false},

		// A single star stays inside one segment.
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/x/c", false},
		{"a/*", "a/b/c", false},

		// "**" also matches nothing at all.
		{"**/x", "x", true},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"**/*.pdf", "src/docs/tmp/y.pdf", true},
		{"**", "anything/at/all", true},

		// A trailing slash is a directory spelling of the same pattern.
		{"alice/.cache/", "alice/.cache", true},
		{"", "", true},
		{"", "x", false},

		// path.Match metacharacters keep working per segment.
		{"a/[bc]/d", "a/b/d", true},
		{"a/[bc]/d", "a/d/d", false},
		{"a/?/d", "a/b/d", true},
		{"a/?/d", "a/bb/d", false},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestMatchAny(t *testing.T) {
	pats := []string{"a/**", "b/*.txt"}
	if !MatchAny(pats, "a/deep/deeper/file") {
		t.Error("MatchAny missed a recursive pattern")
	}
	if !MatchAny(pats, "b/x.txt") {
		t.Error("MatchAny missed a plain pattern")
	}
	if MatchAny(pats, "c/x") {
		t.Error("MatchAny matched an unrelated path")
	}
	if MatchAny(nil, "anything") {
		t.Error("an empty pattern list matched")
	}
}

func TestValidateRejectsMalformedPatterns(t *testing.T) {
	if err := Validate([]string{"a/**/b", "c/*.txt", "d/[ab]/e"}); err != nil {
		t.Fatalf("Validate rejected a valid set: %v", err)
	}
	// A pattern path.Match cannot parse answers "no match", so accepting it
	// silently would turn a typo into an exclusion that never happens.
	err := Validate([]string{"ok/**"}, []string{"bad/[a-"})
	if err == nil {
		t.Fatal("a malformed pattern was accepted")
	}
	if got := err.Error(); got == "" || !contains(got, "bad/[a-") {
		t.Fatalf("error %q does not name the offending pattern", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
