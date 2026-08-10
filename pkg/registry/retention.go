package registry

import (
	"path"
	"sort"
	"strconv"
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
}

// Apply partitions tags into deterministic keep and remove sets. It is pure:
// callers provide now explicitly and it never performs network I/O.
func (p Policy) Apply(tags []TagInfo, now time.Time) (keep, remove []TagInfo) {
	if p.empty() {
		return append([]TagInfo(nil), tags...), nil
	}
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
