package registry

import (
	"errors"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"

	"github.com/manprint/backimage/pkg/index"
)

// TagInfo describes one repository tag. Adapter implementations enrich it
// with the backimage manifest when metadata can be read cheaply.
type TagInfo struct {
	Tag       string          `json:"tag"`
	Digest    v1.Hash         `json:"digest"`
	Created   time.Time       `json:"created"`
	Size      int64           `json:"size"`
	Backimage *index.Manifest `json:"backimage,omitempty"`
}

// Policy describes which tags retention keeps. A tag is kept when any one
// rule selects it; an empty Policy never removes anything.
type Policy struct {
	KeepLast    int
	KeepHourly  int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	KeepYearly  int
	KeepWithin  time.Duration
	KeepTags    []string

	// Scope restricts which tags the policy may remove. A tag whose name does
	// not match is out of scope: it is always kept, and it never counts towards
	// KeepLast or any calendar bucket. Nil means every tag is in scope.
	//
	// Scope is not a rule: it can only narrow what the rules reach. A Policy
	// carrying nothing but a Scope is still empty and removes nothing.
	Scope *regexp.Regexp `json:"-"`
	// GroupBy partitions the in-scope tags: each distinct capture is one group
	// and the rules run independently inside it, so KeepLast means "N per
	// group". A tag the pattern does not match is left ungrouped and kept, like
	// an out-of-scope one. Nil means a single group holding every in-scope tag.
	//
	// GroupBy is not a rule either, for the same reason as Scope.
	GroupBy *regexp.Regexp `json:"-"`
}

// GroupResult is the outcome of the rules inside one group. The rules saw only
// these tags: no other group's content could influence the partition.
type GroupResult struct {
	Key    string    `json:"key"`
	Tags   int       `json:"tags"`
	Keep   []TagInfo `json:"keep,omitempty"`
	Remove []TagInfo `json:"remove,omitempty"`
}

// Plan is the resolved outcome of a policy over one tag list. Keep and Remove
// are the flat partition of the input; Groups and Untouched explain where each
// tag ended up. Both views come from a single resolution, so what a caller
// renders and what it deletes cannot drift apart.
type Plan struct {
	// Total is the number of tags examined.
	Total int
	// InScope is how many of them Scope let the rules reach.
	InScope int
	// Ungrouped is how many in-scope tags GroupBy could not assign to a group.
	// They are kept, exactly like out-of-scope tags.
	Ungrouped int
	// Groups holds the independent retention units in ascending key order.
	Groups []GroupResult
	// Untouched holds the tags no rule was allowed to remove: out of scope, or
	// not matched by GroupBy.
	Untouched []TagInfo
	Keep      []TagInfo
	Remove    []TagInfo
}

// Selected lists the tags the selectors reached, newest first: the in-scope tags
// GroupBy could assign to a group. It excludes Untouched, which is exactly what
// a selector left out on purpose.
func (plan Plan) Selected() []TagInfo {
	out := make([]TagInfo, 0, plan.InScope)
	for _, g := range plan.Groups {
		out = append(out, g.Keep...)
		out = append(out, g.Remove...)
	}
	sortTagsNewest(out)
	return out
}

// finish appends the untouched tags to Keep and puts every list in the same
// newest-first order, so a rendered plan reads the way `repo tags` does.
func (plan Plan) finish() Plan {
	plan.Keep = append(plan.Keep, plan.Untouched...)
	sortTagsNewest(plan.Keep)
	sortTagsNewest(plan.Remove)
	sortTagsNewest(plan.Untouched)
	return plan
}

// CompileTagPattern compiles a user-supplied tag pattern anchored to the whole
// tag name: `db_` matches nothing and `db_.*` matches db_1. Go's regexp is
// unanchored by default, so a bare `db` would also select app_db_1 and mydb_x —
// a silent widening of an irreversible operation. Requiring the pattern to
// cover the tag turns that class of mistake into an empty selection, which the
// caller reports instead of acting on.
//
// The pattern is wrapped in a non-capturing group so that alternation keeps its
// precedence (`a|b` becomes \A(?:a|b)\z, not \Aa|b\z) and the capture group
// indices a caller may rely on are left untouched.
func CompileTagPattern(expr string) (*regexp.Regexp, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, errors.New("il pattern è vuoto; per selezionare ogni tag si scrive '.*'")
	}
	// Compile the raw pattern first: its error message carries offsets that
	// line up with what the user typed, unlike the wrapped one.
	if _, err := regexp.Compile(expr); err != nil {
		return nil, err
	}
	return regexp.Compile(`\A(?:` + expr + `)\z`)
}

// PatternSource recovers the pattern a user typed from a regexp built by
// CompileTagPattern, so a message can echo `db_.*` instead of the anchored
// `\A(?:db_.*)\z` the matcher actually holds. A regexp built any other way is
// returned as it is.
func PatternSource(re *regexp.Regexp) string {
	if re == nil {
		return ""
	}
	src := re.String()
	if inner, ok := strings.CutPrefix(src, `\A(?:`); ok {
		if inner, ok = strings.CutSuffix(inner, `)\z`); ok {
			return inner
		}
	}
	return src
}

// Apply partitions tags into deterministic keep and remove sets. It is pure:
// callers provide now explicitly and it never performs network I/O.
func (p Policy) Apply(tags []TagInfo, now time.Time) (keep, remove []TagInfo) {
	plan := p.PlanFor(tags, now)
	return plan.Keep, plan.Remove
}

// PlanFor resolves the policy and keeps the per-group breakdown alongside the
// flat partition, so a caller can show the plan it is about to execute rather
// than a second, separately computed one. Like Apply it is pure.
func (p Policy) PlanFor(tags []TagInfo, now time.Time) Plan {
	if p.empty() {
		// No rule can remove anything, so no group is resolved and every tag is
		// untouched. The scope counters are still computed honestly: a caller
		// reporting "the pattern reaches 4 of 9" must not be handed 9 of 9 just
		// because there was no rule to apply to those four.
		counted, _, _ := p.partition(tags)
		return Plan{
			Total:     len(tags),
			InScope:   counted.InScope,
			Ungrouped: counted.Ungrouped,
			Keep:      append([]TagInfo(nil), tags...),
			Untouched: append([]TagInfo(nil), tags...),
		}
	}
	plan, buckets, keys := p.partition(tags)
	for _, key := range keys {
		keep, remove := p.applyOne(buckets[key], now)
		plan.Groups = append(plan.Groups, GroupResult{Key: key, Tags: len(buckets[key]), Keep: keep, Remove: remove})
		plan.Keep = append(plan.Keep, keep...)
		plan.Remove = append(plan.Remove, remove...)
	}
	return plan.finish()
}

// Select reports what Scope and GroupBy reach, applying no retention rule at
// all: every selected tag lands in Keep and Remove stays empty. It lets a
// read-only command preview the exact selection a prune would work on, sharing
// the matcher rather than copying it — an approximate preview of an irreversible
// command would be worse than no preview.
func (p Policy) Select(tags []TagInfo) Plan {
	plan, buckets, keys := p.partition(tags)
	for _, key := range keys {
		group := append([]TagInfo(nil), buckets[key]...)
		sortTagsNewest(group)
		plan.Groups = append(plan.Groups, GroupResult{Key: key, Tags: len(group), Keep: group})
		plan.Keep = append(plan.Keep, group...)
	}
	return plan.finish()
}

// partition narrows tags through Scope and splits what remains through GroupBy.
// It runs before any ordering or counting: a tag left in the ordered list would
// consume a KeepLast slot on behalf of a group it does not belong to, and the
// prune would then remove the complement of what was asked.
//
// PlanFor and Select share this step, so the selection a preview shows is by
// construction the selection a prune acts on.
func (p Policy) partition(tags []TagInfo) (Plan, map[string][]TagInfo, []string) {
	plan := Plan{Total: len(tags)}
	inScope := make([]TagInfo, 0, len(tags))
	for _, tag := range tags {
		if p.Scope != nil && !p.Scope.MatchString(tag.Tag) {
			plan.Untouched = append(plan.Untouched, tag)
			continue
		}
		inScope = append(inScope, tag)
	}
	plan.InScope = len(inScope)

	buckets := map[string][]TagInfo{}
	keys := make([]string, 0, len(inScope))
	for _, tag := range inScope {
		key, ok := groupKeyFor(p.GroupBy, tag.Tag)
		if !ok {
			plan.Ungrouped++
			plan.Untouched = append(plan.Untouched, tag)
			continue
		}
		if _, seen := buckets[key]; !seen {
			keys = append(keys, key)
		}
		buckets[key] = append(buckets[key], tag)
	}
	sort.Strings(keys)
	return plan, buckets, keys
}

// groupKeyFor derives the group key of one tag. Without a GroupBy every tag
// belongs to the same group. With one, the key is the concatenation of its
// capture groups, joined by a byte a tag name cannot contain; a tag the pattern
// does not match has no group at all and the caller must keep it.
func groupKeyFor(re *regexp.Regexp, tag string) (string, bool) {
	if re == nil {
		return "", true
	}
	m := re.FindStringSubmatch(tag)
	if m == nil {
		return "", false
	}
	if len(m) == 1 {
		return m[0], true
	}
	return strings.Join(m[1:], "\x00"), true
}

// applyOne runs every rule over one group. It assumes a non-empty policy: the
// caller has already handled the case where no rule exists, which must keep
// everything rather than select nothing.
func (p Policy) applyOne(tags []TagInfo, now time.Time) (keep, remove []TagInfo) {
	ordered := append([]TagInfo(nil), tags...)
	sortTagsNewest(ordered)
	selected := make(map[string]bool, len(ordered))
	mark := func(i int) { selected[tagKey(ordered[i])] = true }

	for i, tag := range ordered {
		// Unknown/non-backimage dates are never pruned by default.
		if tag.Created.IsZero() || matchesAny(tag.Tag, p.KeepTags) {
			mark(i)
		}
		if p.KeepWithin > 0 && !tag.Created.IsZero() && !tag.Created.Before(now.Add(-p.KeepWithin)) {
			mark(i)
		}
	}
	markFirst(ordered, selected, p.KeepLast, tagKey)
	markFirst(ordered, selected, p.KeepHourly, func(t TagInfo) string { return t.Created.UTC().Format("2006010215") })
	markFirst(ordered, selected, p.KeepDaily, func(t TagInfo) string { return t.Created.UTC().Format("20060102") })
	markFirst(ordered, selected, p.KeepWeekly, func(t TagInfo) string {
		y, w := t.Created.UTC().ISOWeek()
		return fmtKey(y, w)
	})
	markFirst(ordered, selected, p.KeepMonthly, func(t TagInfo) string { return t.Created.UTC().Format("200601") })
	markFirst(ordered, selected, p.KeepYearly, func(t TagInfo) string { return t.Created.UTC().Format("2006") })

	for _, tag := range ordered {
		if selected[tagKey(tag)] {
			keep = append(keep, tag)
		} else {
			remove = append(remove, tag)
		}
	}
	return keep, remove
}

func (p Policy) empty() bool {
	return p.KeepLast <= 0 && p.KeepHourly <= 0 && p.KeepDaily <= 0 && p.KeepWeekly <= 0 && p.KeepMonthly <= 0 && p.KeepYearly <= 0 && p.KeepWithin <= 0 && len(p.KeepTags) == 0
}

func sortTagsNewest(tags []TagInfo) {
	sort.SliceStable(tags, func(i, j int) bool {
		if !tags[i].Created.Equal(tags[j].Created) {
			return tags[i].Created.After(tags[j].Created)
		}
		if tags[i].Digest.String() != tags[j].Digest.String() {
			return tags[i].Digest.String() < tags[j].Digest.String()
		}
		return tags[i].Tag < tags[j].Tag
	})
}

func markFirst(tags []TagInfo, selected map[string]bool, n int, bucket func(TagInfo) string) {
	if n <= 0 {
		return
	}
	seen := map[string]bool{}
	kept := 0
	for i, tag := range tags {
		if tag.Created.IsZero() {
			continue
		}
		key := bucket(tag)
		if seen[key] {
			continue
		}
		if kept >= n {
			continue
		}
		seen[key] = true
		kept++
		selected[tagKey(tags[i])] = true
	}
}

func matchesAny(tag string, patterns []string) bool {
	for _, pattern := range patterns {
		ok, err := path.Match(pattern, tag)
		if err == nil && ok {
			return true
		}
	}
	return false
}

func tagKey(t TagInfo) string { return t.Tag + "\x00" + t.Digest.String() }

func fmtKey(year, week int) string { return strconv.Itoa(year) + "-W" + strconv.Itoa(week) }
