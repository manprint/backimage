package cli

import (
	"testing"
	"time"
)

func TestParseHumanDuration(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"0":       0,
		"90s":     90 * time.Second,
		"90m":     90 * time.Minute,
		"12h":     12 * time.Hour,
		"3d":      72 * time.Hour,
		"2w":      14 * 24 * time.Hour,
		"1d12h":   36 * time.Hour,
		"1.5d":    36 * time.Hour,
		"  7d  ":  7 * 24 * time.Hour,
		"1h30m":   90 * time.Minute,
		"1w1d12h": (8*24 + 12) * time.Hour,
	} {
		got, err := parseHumanDuration(value)
		if err != nil {
			t.Fatalf("parseHumanDuration(%q): %v", value, err)
		}
		if got != want {
			t.Fatalf("parseHumanDuration(%q) = %v, want %v", value, got, want)
		}
	}
}

// A bare number is the classic ambiguity this flag type exists to remove:
// nobody can tell whether "3" means seconds, hours or days.
func TestParseHumanDurationRejectsAmbiguous(t *testing.T) {
	for _, value := range []string{"", "3", "abc", "3days", "12hh", "d"} {
		if got, err := parseHumanDuration(value); err == nil {
			t.Fatalf("parseHumanDuration(%q) = %v, want an error", value, got)
		}
	}
}

func TestFormatHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                    "0",
		72 * time.Hour:       "3d",
		14 * 24 * time.Hour:  "2w",
		36 * time.Hour:       "36h0m0s",
		90 * time.Minute:     "1h30m0s",
		7 * 24 * time.Hour:   "1w",
		2 * 24 * time.Hour:   "2d",
		365 * 24 * time.Hour: "365d",
	} {
		if got := formatHumanDuration(d); got != want {
			t.Fatalf("formatHumanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestHumanDurationFlagRoundTrip(t *testing.T) {
	var target time.Duration
	v := newHumanDuration(&target)
	if v.Type() != "duration" {
		t.Fatalf("Type() = %q", v.Type())
	}
	if v.String() != "0" {
		t.Fatalf("zero String() = %q", v.String())
	}
	if err := v.Set("3d"); err != nil {
		t.Fatal(err)
	}
	if target != 72*time.Hour || v.String() != "3d" {
		t.Fatalf("target = %v, String = %q", target, v.String())
	}
	if err := v.Set("nope"); err == nil {
		t.Fatal("Set accepted an invalid duration")
	}
}
