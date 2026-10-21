package registry

import (
	"testing"
	"time"
)

// A malformed --keep-tag glob used to match nothing: as the only keep rule it
// protected nothing, and every dated tag went to removal.
func TestPolicyValidateRefusesKeepTagsThatMatchNothing(t *testing.T) {
	for _, pattern := range []string{"release-[", "a\\", "", "[]a"} {
		if err := (Policy{KeepTags: []string{"release-*", pattern}}).Validate(); err == nil {
			t.Fatalf("keep-tag %q accepted", pattern)
		}
	}
	if err := (Policy{KeepTags: []string{"release-*", "v[0-9]*"}, KeepLast: 2}).Validate(); err != nil {
		t.Fatalf("valid policy refused: %v", err)
	}
}

// A policy that reaches PlanFor without Validate must fail safe: a keep rule
// whose pattern cannot be evaluated keeps, it does not delete.
func TestMalformedKeepTagKeepsInsteadOfRemoving(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := retentionTags(5, now)
	plan := (Policy{KeepTags: []string{"release-["}}).PlanFor(tags, now)
	if len(plan.Remove) != 0 {
		t.Fatalf("a malformed keep-tag removed %d tags", len(plan.Remove))
	}
}
