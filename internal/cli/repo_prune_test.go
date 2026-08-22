package cli

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/manprint/backimage/pkg/registry"
)

func TestPruneDurationFlagsAcceptDaysAndHours(t *testing.T) {
	cmd := newRepoCommand()
	prune, _, err := cmd.Find([]string{"prune"})
	if err != nil {
		t.Fatal(err)
	}
	if err := prune.ParseFlags([]string{"--delete-older-than", "3d", "--keep-last", "2"}); err != nil {
		t.Fatal(err)
	}
	got, set, err := pruneDuration(prune, "delete-older-than")
	if err != nil || !set || got != 72*time.Hour {
		t.Fatalf("delete-older-than = %v, set=%v, err=%v", got, set, err)
	}
	if _, set, err := pruneDuration(prune, "keep-within"); err != nil || set {
		t.Fatalf("keep-within reported as set: %v, %v", set, err)
	}

	cmd = newRepoCommand()
	prune, _, _ = cmd.Find([]string{"prune"})
	if err := prune.ParseFlags([]string{"--keep-within", "12h"}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := pruneDuration(prune, "keep-within"); err != nil || got != 12*time.Hour {
		t.Fatalf("keep-within = %v, %v", got, err)
	}

	cmd = newRepoCommand()
	prune, _, _ = cmd.Find([]string{"prune"})
	if err := prune.ParseFlags([]string{"--keep-within", "7"}); err == nil {
		t.Fatal("a unit-less duration was accepted")
	}
}

func TestRepoPruneRejectsBothDurationFlags(t *testing.T) {
	_, _, err := runRoot(t, "repo", "prune", "example.com/me/x",
		"--keep-within", "1d", "--delete-older-than", "1d", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "same rule") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepoPruneRejectsZeroDuration(t *testing.T) {
	for _, flag := range []string{"--keep-within", "--delete-older-than"} {
		t.Run(flag, func(t *testing.T) {
			_, _, err := runRoot(t, "repo", "prune", "example.com/me/x", flag, "0", "--dry-run")
			if err == nil || !strings.Contains(err.Error(), "greater than zero") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPrunePlanText(t *testing.T) {
	created := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	tag := func(name string, when time.Time) registry.TagInfo {
		return registry.TagInfo{Tag: name, Created: when, Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("ab", 32)}}
	}

	// No rule at all: the command must say so instead of printing an empty list.
	empty := prunePlanText(false, registry.Policy{}, registry.Plan{})
	if !strings.Contains(empty, "nessuna regola") {
		t.Fatalf("no-rule text = %q", empty)
	}

	p := registry.Policy{KeepLast: 2, KeepWithin: 72 * time.Hour, KeepTags: []string{"release-*"}}
	dry := prunePlanText(true, p, registry.Plan{Keep: []registry.TagInfo{tag("keep", created)}, Remove: []registry.TagInfo{tag("old", created)}})
	for _, want := range []string{"mantieni i 2 più recenti", "mantieni più recenti di 3d", "release-*", "dry-run", "old", "2026-08-01T10:00:00Z"} {
		if !strings.Contains(dry, want) {
			t.Fatalf("dry-run text %q missing %q", dry, want)
		}
	}

	applied := prunePlanText(false, p, registry.Plan{Remove: []registry.TagInfo{tag("old", time.Time{})}})
	if strings.Contains(applied, "dry-run") {
		t.Fatalf("applied text mentions dry-run: %q", applied)
	}
	// An undated tag must not be rendered as year 1.
	if !strings.Contains(applied, "\t-\t") {
		t.Fatalf("undated tag rendered as %q", applied)
	}

	none := prunePlanText(false, p, registry.Plan{Keep: []registry.TagInfo{tag("a", created)}})
	if !strings.Contains(none, "nessun tag da eliminare") {
		t.Fatalf("empty removal text = %q", none)
	}
}
