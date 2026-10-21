package server

import (
	"strings"
	"testing"
)

// TestRepoAllowedStopsAtAPathBoundary is the regression for the allowlist
// that compared raw string prefixes: "team/a" admitted "team/abc", a
// repository the operator never listed.
func TestRepoAllowedStopsAtAPathBoundary(t *testing.T) {
	for _, tc := range []struct {
		prefix, ref string
		want        bool
	}{
		{"ghcr.io/team/a", "ghcr.io/team/a:latest", true},
		{"ghcr.io/team/a", "ghcr.io/team/a@sha256:00", true},
		{"ghcr.io/team/a", "ghcr.io/team/a/sub:1", true},
		{"ghcr.io/team/a", "ghcr.io/team/a", true},
		{"ghcr.io/team/a", "ghcr.io/team/abc:latest", false},
		{"ghcr.io/team/a", "ghcr.io/team/a-other:1", false},
		{"ghcr.io/team/", "ghcr.io/team/abc:1", true},
		{"ghcr.io/team/", "ghcr.io/teammate/x:1", false},
		{"ghcr.io", "ghcr.io/team/x:1", true},
		{"ghcr.io", "ghcr.io.evil.example/x:1", false},
		// A host prefix names a registry, port included.
		{"localhost", "localhost:5000/x:1", false},
		{"localhost:5000", "localhost:5000/x:1", true},
		{"localhost:5000/", "localhost:5000/x:1", true},
	} {
		if got := repoAllowed(tc.ref, []string{tc.prefix}); got != tc.want {
			t.Errorf("repoAllowed(%q, %q) = %v, want %v", tc.ref, tc.prefix, got, tc.want)
		}
	}
	if !repoAllowed("anything:1", nil) {
		t.Error("an empty allowlist must allow every repository")
	}
}

// TestAllowedReposRefusesPatternsThatMatchNothing: a glob or an empty entry
// is not a prefix. "ghcr.io/me/*", the form the README used to show,
// compared literally and refused every backup; an empty entry admitted all.
func TestAllowedReposRefusesPatternsThatMatchNothing(t *testing.T) {
	for _, bad := range []string{"", "  ", "ghcr.io/me/*", "ghcr.io/me/?", "ghcr.io/[ab]/"} {
		if err := ValidateAllowedRepos([]string{"ghcr.io/ok/", bad}); err == nil {
			t.Errorf("ValidateAllowedRepos accepted %q", bad)
		}
	}
	if err := ValidateAllowedRepos([]string{"ghcr.io/team/", "localhost:5000/e2e/"}); err != nil {
		t.Fatalf("ValidateAllowedRepos refused valid prefixes: %v", err)
	}
	_, err := NewSession(SessionConfig{AllowNoAuth: true, AllowedRepos: []string{"ghcr.io/me/*"}}, newStreamSink())
	if err == nil || !strings.Contains(err.Error(), "ghcr.io/me/*") {
		t.Fatalf("NewSession with a glob prefix = %v, want a refusal naming it", err)
	}
}
