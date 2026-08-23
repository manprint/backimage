package backup

import "testing"

// TestTempSpaceNeededCoversEveryRetainedLayer pins the preflight to what the
// pipeline really does. rollLayer drops the spool but keeps the file
// NewFileLayer produced, and cleanup only runs when Run returns, so every
// layer is on disk at once while the push happens. The old rule,
// jobs x max-layer-size, described a window that does not exist: measured on a
// 1 GiB incompressible source with --jobs 1 --max-layer-size 64MiB, it asked
// for 64 MiB while the real peak was 1024 MiB.
func TestTempSpaceNeededCoversEveryRetainedLayer(t *testing.T) {
	const (
		raw   = 20 << 30 // a 20 GiB source
		layer = 1 << 30  // the default --max-layer-size
	)
	need := tempSpaceNeeded(raw, layer)
	if need < raw {
		t.Fatalf("need = %d, want at least the stored upper bound %d", need, raw)
	}
	// The old rule, for reference: three jobs of one layer each.
	if old := int64(3) * layer; need <= old {
		t.Fatalf("need = %d, no larger than the window rule %d it replaces", need, old)
	}
}

// TestTempSpaceNeededIgnoresJobs is the point of the change: concurrency does
// not bound the peak, because nothing is released between layers.
func TestTempSpaceNeededIgnoresJobs(t *testing.T) {
	const raw, layer = 4 << 30, 512 << 20
	if a, b := tempSpaceNeeded(raw, layer), tempSpaceNeeded(raw, layer); a != b {
		t.Fatalf("not deterministic: %d != %d", a, b)
	}
	// A smaller layer no longer buys a smaller requirement beyond the two
	// transient files, which is why --max-layer-size is no longer offered as
	// the remedy.
	big := tempSpaceNeeded(raw, layer)
	small := tempSpaceNeeded(raw, layer/8)
	if big-small > 2*layer {
		t.Fatalf("layer size still dominates: %d vs %d", big, small)
	}
	if small < raw {
		t.Fatalf("small = %d, below the stored upper bound %d", small, raw)
	}
}
