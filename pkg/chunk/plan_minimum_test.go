package chunk

import (
	"strings"
	"testing"
)

// A --max-layer-size under the 16 MiB minimum was raised without a word: a
// user asking for 4 MiB layers got 16 MiB ones and no way to tell.
func TestATargetUnderTheMinimumIsReported(t *testing.T) {
	limits := DefaultLimits()
	limits.TargetLayerBytes = 4 << 20
	p, err := PlanLayers(40<<20, limits)
	if err != nil {
		t.Fatal(err)
	}
	if p.LayerBytes != limits.MinLayerBytes {
		t.Fatalf("LayerBytes = %d, want the %d minimum", p.LayerBytes, limits.MinLayerBytes)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "--max-layer-size") {
		t.Fatalf("warnings = %v, want one naming --max-layer-size", p.Warnings)
	}
	// The default target never reaches the floor: a small input alone does
	// not warn.
	if p, _ := PlanLayers(1<<20, DefaultLimits()); len(p.Warnings) != 0 {
		t.Fatalf("a small backup at the default target warned: %v", p.Warnings)
	}
}
