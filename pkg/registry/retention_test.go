package registry

import (
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"strings"
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

// --- Selectors: whole-tag anchoring ------------------------------------------

func TestCompileTagPatternAnchorsWholeTag(t *testing.T) {
	cases := []struct {
		expr string
		tag  string
		want bool
	}{
		// The footgun the anchoring exists for: unanchored, `db_` would select
		// app_db_1 and mydb_1 too, silently widening an irreversible operation.
		{`db_`, "db_1", false},
		{`db_.*`, "db_1", true},
		{`db_.*`, "app_db_1", false},
		{`db_.*`, "mydb_1", false},
		{`.*db.*`, "app_db_1", true},
		// Alternation must keep its precedence through the wrapping.
		{`db_.*|app_.*`, "db_1", true},
		{`db_.*|app_.*`, "app_1", true},
		{`db_.*|app_.*`, "web_1", false},
		// A pattern the user already anchored stays correct.
		{`^db_.*$`, "db_1", true},
		// Inline flags still work.
		{`(?i)DB_.*`, "db_1", true},
	}
	for _, c := range cases {
		re, err := CompileTagPattern(c.expr)
		if err != nil {
			t.Fatalf("CompileTagPattern(%q) = %v", c.expr, err)
		}
		if got := re.MatchString(c.tag); got != c.want {
			t.Errorf("%q matches %q = %v, want %v", c.expr, c.tag, got, c.want)
		}
	}

	if _, err := CompileTagPattern(`db_[`); err == nil {
		t.Fatal("an invalid pattern was accepted")
	}
	for _, blank := range []string{"", "   "} {
		if _, err := CompileTagPattern(blank); err == nil {
			t.Fatalf("the blank pattern %q was accepted", blank)
		}
	}
}

func TestCompileTagPatternPreservesCaptureGroups(t *testing.T) {
	re, err := CompileTagPattern(`([a-z]+)_([0-9]+)`)
	if err != nil {
		t.Fatal(err)
	}
	// The wrapping group must be non-capturing, or a caller counting capture
	// groups would see one that the user did not write.
	if re.NumSubexp() != 2 {
		t.Fatalf("NumSubexp = %d, want 2", re.NumSubexp())
	}
	m := re.FindStringSubmatch("db_12")
	if len(m) != 3 || m[1] != "db" || m[2] != "12" {
		t.Fatalf("submatches = %#v", m)
	}
}

func TestPatternSourceEchoesWhatTheUserTyped(t *testing.T) {
	re, err := CompileTagPattern(`db_.*|app_.*`)
	if err != nil {
		t.Fatal(err)
	}
	if got := PatternSource(re); got != `db_.*|app_.*` {
		t.Fatalf("PatternSource = %q", got)
	}
	if got := PatternSource(nil); got != "" {
		t.Fatalf("PatternSource(nil) = %q", got)
	}
	// A regexp built elsewhere is echoed as it is rather than mangled.
	plain := regexp.MustCompile(`db_.*`)
	if got := PatternSource(plain); got != `db_.*` {
		t.Fatalf("PatternSource(plain) = %q", got)
	}
}

// --- Invariant 1: a selector is never a deletion rule ------------------------

func TestScopeAloneRemovesNothing(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := retentionTags(20, now)
	for _, p := range []Policy{
		{Scope: mustPattern(t, `daily-0.*`)},
		{GroupBy: mustPattern(t, `(daily)-.*`)},
		{Scope: mustPattern(t, `daily-0.*`), GroupBy: mustPattern(t, `(daily)-.*`)},
	} {
		keep, remove := p.Apply(tags, now)
		if len(remove) != 0 || len(keep) != len(tags) {
			t.Fatalf("a selector without a rule removed %d tag(s)", len(remove))
		}
		if p.empty() != true {
			t.Fatal("a policy holding only selectors is not reported as empty")
		}
	}
}

// --- Invariant 2: out-of-scope tags never consume a keep slot ----------------

// The worst possible bug of the feature: narrowing after the ordering makes
// --tag-regex 'db_.*' --keep-last 3 keep the three newest tags of the whole
// repository — which are app_* here — and delete every db_*, i.e. exactly the
// complement of what was asked, with no error at all.
func TestScopeDoesNotBorrowKeepSlotsFromOtherTags(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var tags []TagInfo
	for i := 1; i <= 4; i++ { // app_* are the newest tags in the repository
		tags = append(tags, TagInfo{Tag: fmt.Sprintf("app_%d", i), Created: now.Add(-time.Duration(i) * time.Hour)})
	}
	for i := 1; i <= 4; i++ {
		tags = append(tags, TagInfo{Tag: fmt.Sprintf("db_%d", i), Created: now.AddDate(0, 0, -i)})
	}
	p := Policy{Scope: mustPattern(t, `db_.*`), KeepLast: 3}
	keep, remove := p.Apply(tags, now)

	if got := tagNames(remove); len(got) != 1 || got[0] != "db_4" {
		t.Fatalf("remove = %v, want exactly [db_4]", got)
	}
	for _, tag := range remove {
		if strings.HasPrefix(tag.Tag, "app_") {
			t.Fatalf("an out-of-scope tag was removed: %v", tagNames(remove))
		}
	}
	if len(keep) != 7 {
		t.Fatalf("keep = %v, want 7 tags", tagNames(keep))
	}
}

// --- Invariant 3: an undated tag stays kept, in scope as much as out --------

func TestScopeKeepsUndatedTags(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := []TagInfo{
		{Tag: "db_1", Created: now.AddDate(0, 0, -1)},
		{Tag: "db_2", Created: now.AddDate(0, 0, -2)},
		{Tag: "db_undated"},
	}
	p := Policy{Scope: mustPattern(t, `db_.*`), KeepLast: 1}
	_, remove := p.Apply(tags, now)
	if got := tagNames(remove); len(got) != 1 || got[0] != "db_2" {
		t.Fatalf("remove = %v, want exactly [db_2]", got)
	}
}

// --- Grouping ---------------------------------------------------------------

func TestGroupByAppliesRulesPerGroup(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var tags []TagInfo
	for _, family := range []string{"db", "app"} {
		for i := 1; i <= 4; i++ {
			tags = append(tags, TagInfo{Tag: fmt.Sprintf("%s_%d", family, i), Created: now.AddDate(0, 0, -i)})
		}
	}
	p := Policy{GroupBy: mustPattern(t, `([a-z]+)_.*`), KeepLast: 3}
	plan := p.PlanFor(tags, now)

	if len(plan.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(plan.Groups))
	}
	// Ascending key order, so the rendering is reproducible.
	if plan.Groups[0].Key != "app" || plan.Groups[1].Key != "db" {
		t.Fatalf("group keys = %q, %q", plan.Groups[0].Key, plan.Groups[1].Key)
	}
	want := []string{"app_4", "db_4"}
	if got := tagNames(plan.Remove); !slices.Equal(got, want) {
		t.Fatalf("remove = %v, want %v", got, want)
	}
	for _, g := range plan.Groups {
		if g.Tags != 4 || len(g.Keep) != 3 || len(g.Remove) != 1 {
			t.Fatalf("group %q = %d tags, %d keep, %d remove", g.Key, g.Tags, len(g.Keep), len(g.Remove))
		}
	}
}

// Each group must be resolved as if the others did not exist: adding tags to one
// family cannot change what happens to another.
func TestGroupsAreIndependent(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	db := func() []TagInfo {
		var out []TagInfo
		for i := 1; i <= 4; i++ {
			out = append(out, TagInfo{Tag: fmt.Sprintf("db_%d", i), Created: now.AddDate(0, 0, -i)})
		}
		return out
	}
	p := Policy{GroupBy: mustPattern(t, `([a-z]+)_.*`), KeepLast: 2}

	alone := p.PlanFor(db(), now)
	crowded := db()
	for i := 1; i <= 40; i++ { // a much larger, much newer family
		crowded = append(crowded, TagInfo{Tag: fmt.Sprintf("app_%d", i), Created: now.Add(-time.Duration(i) * time.Minute)})
	}
	together := p.PlanFor(crowded, now)

	dbRemoved := func(plan Plan) []string {
		var out []string
		for _, g := range plan.Groups {
			if g.Key == "db" {
				out = append(out, tagNames(g.Remove)...)
			}
		}
		return out
	}
	if a, b := dbRemoved(alone), dbRemoved(together); !slices.Equal(a, b) {
		t.Fatalf("group db removed %v alone but %v alongside app", a, b)
	}
}

func TestGroupByKeepsUngroupedTags(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := []TagInfo{
		{Tag: "db_1", Created: now.AddDate(0, 0, -1)},
		{Tag: "db_2", Created: now.AddDate(0, 0, -2)},
		{Tag: "latest", Created: now.AddDate(0, 0, -9)}, // no family: not grouped
	}
	p := Policy{GroupBy: mustPattern(t, `([a-z]+)_.*`), KeepLast: 1}
	plan := p.PlanFor(tags, now)
	if plan.Ungrouped != 1 {
		t.Fatalf("Ungrouped = %d, want 1", plan.Ungrouped)
	}
	if got := tagNames(plan.Remove); !slices.Equal(got, []string{"db_2"}) {
		t.Fatalf("remove = %v, want [db_2]", got)
	}
	if got := tagNames(plan.Untouched); !slices.Equal(got, []string{"latest"}) {
		t.Fatalf("untouched = %v, want [latest]", got)
	}
}

// A group-by pattern that does not cover the whole tag matches nothing, so it
// deletes nothing instead of grouping wrongly.
func TestGroupByThatDoesNotCoverTheTagSelectsNothing(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := []TagInfo{
		{Tag: "db_1", Created: now.AddDate(0, 0, -1)},
		{Tag: "db_2", Created: now.AddDate(0, 0, -2)},
	}
	p := Policy{GroupBy: mustPattern(t, `([a-z]+)_`), KeepLast: 1}
	plan := p.PlanFor(tags, now)
	if len(plan.Groups) != 0 || len(plan.Remove) != 0 || plan.Ungrouped != 2 {
		t.Fatalf("groups=%d remove=%v ungrouped=%d", len(plan.Groups), tagNames(plan.Remove), plan.Ungrouped)
	}
}

// --- Select is the same selection PlanFor acts on ---------------------------

func TestSelectMatchesWhatPlanForReaches(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var tags []TagInfo
	for _, family := range []string{"db", "app", "web"} {
		for i := 1; i <= 5; i++ {
			tags = append(tags, TagInfo{Tag: fmt.Sprintf("%s_%d", family, i), Created: now.AddDate(0, 0, -i)})
		}
	}
	tags = append(tags, TagInfo{Tag: "latest", Created: now})

	for _, sel := range []Policy{
		{Scope: mustPattern(t, `db_.*`)},
		{GroupBy: mustPattern(t, `([a-z]+)_.*`)},
		{Scope: mustPattern(t, `db_.*|app_.*`), GroupBy: mustPattern(t, `([a-z]+)_.*`)},
	} {
		preview := sel.Select(tags)
		acting := Policy{Scope: sel.Scope, GroupBy: sel.GroupBy, KeepLast: 2}.PlanFor(tags, now)
		if a, b := tagNames(preview.Selected()), tagNames(acting.Selected()); !slices.Equal(a, b) {
			t.Fatalf("preview selected %v but the prune would reach %v", a, b)
		}
		if preview.InScope != acting.InScope || preview.Ungrouped != acting.Ungrouped {
			t.Fatalf("preview %d/%d vs acting %d/%d", preview.InScope, preview.Ungrouped, acting.InScope, acting.Ungrouped)
		}
		if len(preview.Remove) != 0 {
			t.Fatalf("Select removed %d tag(s)", len(preview.Remove))
		}
	}
}

// --- Determinism and the complete/disjoint partition, with selectors --------

func TestSelectorPlansAreDeterministic(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var tags []TagInfo
	for _, family := range []string{"db", "app"} {
		for i := 1; i <= 9; i++ {
			tags = append(tags, TagInfo{Tag: fmt.Sprintf("%s_%d", family, i), Created: now.AddDate(0, 0, -i)})
		}
	}
	p := Policy{Scope: mustPattern(t, `db_.*|app_.*`), GroupBy: mustPattern(t, `([a-z]+)_.*`), KeepLast: 3}
	first := p.PlanFor(tags, now)
	for n := 0; n < 20; n++ {
		again := p.PlanFor(tags, now)
		if !slices.Equal(tagNames(first.Remove), tagNames(again.Remove)) {
			t.Fatalf("run %d removed a different set", n)
		}
		if !slices.Equal(groupKeys(first), groupKeys(again)) {
			t.Fatalf("run %d ordered the groups differently", n)
		}
	}
}

func TestRetentionPartitionsStayCompleteWithSelectors(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	r := rand.New(rand.NewSource(29))
	families := []string{"db", "app", "web", "misc"}
	selectors := []struct{ scope, groupBy string }{
		{"", ""},
		{`db_.*`, ""},
		{"", `([a-z]+)_.*`},
		{`db_.*|app_.*`, `([a-z]+)_.*`},
		{`nomatch_.*`, `([a-z]+)_.*`},
	}
	for n := 0; n < 400; n++ {
		tags := make([]TagInfo, n%53)
		for i := range tags {
			family := families[r.Intn(len(families))]
			created := now.Add(-time.Duration(r.Intn(24*400)) * time.Hour)
			if r.Intn(11) == 0 {
				created = time.Time{} // undated tags must survive every path
			}
			tags[i] = TagInfo{Tag: fmt.Sprintf("%s_%d", family, i), Created: created}
		}
		if n%7 == 0 && len(tags) > 0 {
			tags[0].Tag = "latest" // ungroupable, and out of every scope
		}
		sel := selectors[n%len(selectors)]
		p := Policy{
			KeepLast:   r.Intn(5),
			KeepDaily:  r.Intn(4),
			KeepWithin: time.Duration(r.Intn(40)) * 24 * time.Hour,
		}
		if sel.scope != "" {
			p.Scope = mustPattern(t, sel.scope)
		}
		if sel.groupBy != "" {
			p.GroupBy = mustPattern(t, sel.groupBy)
		}
		plan := p.PlanFor(tags, now)
		if len(plan.Keep)+len(plan.Remove) != len(tags) {
			t.Fatalf("case %d lost tags: %d + %d != %d", n, len(plan.Keep), len(plan.Remove), len(tags))
		}
		seen := map[string]bool{}
		for _, tag := range append(append([]TagInfo(nil), plan.Keep...), plan.Remove...) {
			if seen[tag.Tag] {
				t.Fatalf("case %d duplicated %q across keep and remove", n, tag.Tag)
			}
			seen[tag.Tag] = true
		}
		// Nothing the selectors excluded may ever be removed, and no undated tag
		// may be removed either.
		for _, tag := range plan.Remove {
			if p.Scope != nil && !p.Scope.MatchString(tag.Tag) {
				t.Fatalf("case %d removed the out-of-scope tag %q", n, tag.Tag)
			}
			if tag.Created.IsZero() {
				t.Fatalf("case %d removed the undated tag %q", n, tag.Tag)
			}
		}
		if p.empty() {
			// With no rule the policy short-circuits: no group is reported,
			// because there was never anything to decide.
			if len(plan.Groups) != 0 || len(plan.Remove) != 0 {
				t.Fatalf("case %d: an empty policy produced %d group(s) and %d removal(s)", n, len(plan.Groups), len(plan.Remove))
			}
			continue
		}
		// The group breakdown must account for every in-scope grouped tag.
		total := 0
		for _, g := range plan.Groups {
			if g.Tags != len(g.Keep)+len(g.Remove) {
				t.Fatalf("case %d group %q: %d tags but %d+%d in the partition", n, g.Key, g.Tags, len(g.Keep), len(g.Remove))
			}
			total += g.Tags
		}
		if total != plan.InScope-plan.Ungrouped {
			t.Fatalf("case %d: groups hold %d tags, in scope %d minus ungrouped %d", n, total, plan.InScope, plan.Ungrouped)
		}
	}
}

func mustPattern(t *testing.T, expr string) *regexp.Regexp {
	t.Helper()
	re, err := CompileTagPattern(expr)
	if err != nil {
		t.Fatalf("CompileTagPattern(%q) = %v", expr, err)
	}
	return re
}

func tagNames(tags []TagInfo) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		out = append(out, tag.Tag)
	}
	return out
}

func groupKeys(plan Plan) []string {
	out := make([]string, 0, len(plan.Groups))
	for _, g := range plan.Groups {
		out = append(out, g.Key)
	}
	return out
}

// The CLI refuses a GroupBy without a capture group, but the library must still
// behave predictably: the whole tag becomes the key, so every tag is its own
// group and the rules keep all of them.
func TestGroupByWithoutCaptureGroupKeysOnTheWholeTag(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := []TagInfo{
		{Tag: "db_1", Created: now.AddDate(0, 0, -1)},
		{Tag: "db_2", Created: now.AddDate(0, 0, -2)},
	}
	plan := Policy{GroupBy: mustPattern(t, `db_.*`), KeepLast: 1}.PlanFor(tags, now)
	if len(plan.Groups) != 2 {
		t.Fatalf("groups = %d, want one per tag", len(plan.Groups))
	}
	if len(plan.Remove) != 0 {
		t.Fatalf("remove = %v, want nothing", tagNames(plan.Remove))
	}
}

// With no rule nothing is removed, but the scope counters must still describe
// what the pattern reaches: a caller rendering them would otherwise report the
// whole repository as selected.
func TestEmptyPolicyStillCountsTheScopeHonestly(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var tags []TagInfo
	for _, family := range []string{"db", "app"} {
		for i := 1; i <= 4; i++ {
			tags = append(tags, TagInfo{Tag: fmt.Sprintf("%s_%d", family, i), Created: now.AddDate(0, 0, -i)})
		}
	}
	tags = append(tags, TagInfo{Tag: "latest", Created: now})

	plan := Policy{Scope: mustPattern(t, `db_.*`)}.PlanFor(tags, now)
	if plan.Total != 9 || plan.InScope != 4 {
		t.Fatalf("Total=%d InScope=%d, want 9 and 4", plan.Total, plan.InScope)
	}
	if len(plan.Remove) != 0 || len(plan.Keep) != 9 || len(plan.Groups) != 0 {
		t.Fatalf("remove=%d keep=%d groups=%d", len(plan.Remove), len(plan.Keep), len(plan.Groups))
	}
	// The keep list of an empty policy keeps the input order, as it always has.
	if plan.Keep[0].Tag != "db_1" || plan.Keep[len(plan.Keep)-1].Tag != "latest" {
		t.Fatalf("the empty policy reordered its keep list: %v", tagNames(plan.Keep))
	}

	grouped := Policy{GroupBy: mustPattern(t, `([a-z]+)_.*`)}.PlanFor(tags, now)
	if grouped.Ungrouped != 1 {
		t.Fatalf("Ungrouped = %d, want 1 (latest)", grouped.Ungrouped)
	}
}
