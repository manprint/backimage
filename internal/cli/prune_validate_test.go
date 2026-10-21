package cli

import (
	"strings"
	"testing"
)

// A malformed --keep-tag glob protected nothing: with --yes every dated tag
// was deleted. It is a usage error, and it is found before the registry is
// contacted (127.0.0.1:1 refuses connections: reaching it would be exit 6).
func TestRepoPruneRefusesAMalformedKeepTagBeforeTheNetwork(t *testing.T) {
	for _, pattern := range []string{"release-[", ",release-*"} {
		_, _, err := runRoot(t, "repo", "prune", "127.0.0.1:1/me/x", "--keep-tag", pattern, "--dry-run")
		if ExitCodeFor(err) != int(KindUsage) || !strings.Contains(err.Error(), "--keep-tag") {
			t.Fatalf("--keep-tag %q: exit %d (%v), want %d", pattern, ExitCodeFor(err), err, KindUsage)
		}
	}
}

// A duration past the int64 range wrapped: 10000000w came out negative and
// 15250w15250w5d about one day, so --keep-within kept only the last day.
func TestParseHumanDurationRefusesOverflow(t *testing.T) {
	for _, value := range []string{"10000000w", "15250w15250w5d", "106752d", "106751d24h", "-10000000w", "15250w15250w"} {
		if d, err := parseHumanDuration(value); err == nil {
			t.Fatalf("%q = %v, want an out-of-range error", value, d)
		}
	}
	// The largest representable day count still parses.
	if d, err := parseHumanDuration("106751d"); err != nil || d <= 0 {
		t.Fatalf("106751d = %v, %v", d, err)
	}
}
