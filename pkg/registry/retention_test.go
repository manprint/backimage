package registry

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func retentionTags(n int, now time.Time) []TagInfo {
	tags := make([]TagInfo, n)
	for i := range tags {
		tags[i] = TagInfo{Tag: fmt.Sprintf("daily-%03d", i), Created: now.AddDate(0, 0, -i)}
	}
	return tags
}

func TestRetentionRules(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := retentionTags(100, now)
	keep, remove := (Policy{KeepDaily: 7}).Apply(tags, now)
	if len(keep) != 7 || len(remove) != 93 {
		t.Fatalf("daily retention = %d keep, %d remove", len(keep), len(remove))
	}
	for i, tag := range keep {
		if tag.Tag != fmt.Sprintf("daily-%03d", i) {
			t.Fatalf("daily order[%d] = %q", i, tag.Tag)
		}
	}
	keep, _ = (Policy{KeepLast: 3}).Apply(tags, now)
	if len(keep) != 3 {
		t.Fatalf("keep-last = %d, want 3", len(keep))
	}

	monthly := retentionTags(240, now)
	keep, _ = (Policy{KeepLast: 3, KeepMonthly: 6}).Apply(monthly, now)
	if len(keep) < 6 {
		t.Fatalf("union keep-last/monthly unexpectedly small: %d", len(keep))
	}
	keep, _ = (Policy{KeepWithin: 30 * 24 * time.Hour}).Apply(tags, now)
	if len(keep) != 31 {
		t.Fatalf("keep-within = %d, want 31", len(keep))
	}
	protected := append(tags, TagInfo{Tag: "release-v1", Created: now.AddDate(0, 0, -365)})
	keep, _ = (Policy{KeepLast: 1, KeepTags: []string{"release-*"}}).Apply(protected, now)
	if len(keep) != 2 {
		t.Fatalf("protected tag was not retained: %+v", keep)
	}
	keep, remove = (Policy{}).Apply(tags, now)
	if len(keep) != len(tags) || len(remove) != 0 {
		t.Fatalf("empty policy removed tags: %d keep %d remove", len(keep), len(remove))
	}
}

func TestRetentionPartitionsAreCompleteAndDisjoint(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	r := rand.New(rand.NewSource(11))
	for n := 0; n < 500; n++ {
		tags := make([]TagInfo, n%97)
		for i := range tags {
			tags[i] = TagInfo{Tag: fmt.Sprintf("t-%d", i), Created: now.Add(-time.Duration(r.Intn(24*400)) * time.Hour)}
		}
		policy := Policy{KeepLast: r.Intn(8), KeepDaily: r.Intn(8), KeepWeekly: r.Intn(4), KeepWithin: time.Duration(r.Intn(60)) * 24 * time.Hour}
		keep, remove := policy.Apply(tags, now)
		if len(keep)+len(remove) != len(tags) {
			t.Fatalf("case %d lost tags", n)
		}
		seen := map[string]bool{}
		for _, tag := range append(keep, remove...) {
			if seen[tag.Tag] {
				t.Fatalf("case %d duplicated %q", n, tag.Tag)
			}
			seen[tag.Tag] = true
		}
	}
}
