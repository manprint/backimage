package chunk

import (
	"math/rand"
	"testing"
)

func TestPlanLayersTable(t *testing.T) {
	const GiB = 1 << 30
	const MiB = 1 << 20
	def := DefaultLimits()
	minL := def.MinLayerBytes

	cases := []struct {
		name      string
		est       int64
		wantCount int
		wantBytes int64
		wantWarns int
		limits    LayerLimits
	}{
		{"zero", 0, 1, minL, 0, def},
		{"small-below-target", 100 * MiB, 1, 100 * MiB, 0, def},
		{"fifty-gib", 50 * GiB, 50, GiB, 0, def},
		{"five-hundred-gib", 500 * GiB, 118, 4549753492, 1, def},
		{"two-tib", 2 * 1024 * GiB, 118, 18635790302, 2, def},
		{"ten-mib", 10 * MiB, 1, minL, 0, def},
		{"single-layer-limit", 500 * GiB, 1, 500 * GiB, 1, LayerLimits{
			MaxDataLayers: 1, MaxLayerBytes: 1 << 40, MinLayerBytes: minL, TargetLayerBytes: GiB,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := PlanLayers(tc.est, tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			if p.LayerCount != tc.wantCount {
				t.Fatalf("LayerCount = %d, want %d", p.LayerCount, tc.wantCount)
			}
			if p.LayerBytes != tc.wantBytes {
				t.Fatalf("LayerBytes = %d, want %d", p.LayerBytes, tc.wantBytes)
			}
			if len(p.Warnings) != tc.wantWarns {
				t.Fatalf("warnings = %d (%v), want %d", len(p.Warnings), p.Warnings, tc.wantWarns)
			}
			if p.ChunkBytes < MinChunkSize || p.ChunkBytes > 64<<20 {
				t.Fatalf("ChunkBytes = %d out of [1 MiB, 64 MiB]", p.ChunkBytes)
			}
		})
	}
}

func TestPlanLayersInvariant(t *testing.T) {
	def := DefaultLimits()
	r := rand.New(rand.NewSource(2026))
	for i := 0; i < 200; i++ {
		est := int64(1<<10) + r.Int63n(100<<40) // 1 KiB .. 100 TiB
		p, err := PlanLayers(est, def)
		if err != nil {
			t.Fatal(err)
		}
		if p.LayerCount <= 0 || p.LayerCount > def.MaxDataLayers {
			t.Fatalf("est=%d: LayerCount = %d exceeds max", est, p.LayerCount)
		}
		held := int64(p.LayerCount) * p.LayerBytes
		if held < est {
			t.Fatalf("est=%d: layers hold %d*%d = %d < est", est, p.LayerCount, p.LayerBytes, held)
		}
	}
}

func TestPlanLayersBadLimits(t *testing.T) {
	for _, lim := range []LayerLimits{
		{MaxDataLayers: 0},
		{MaxDataLayers: 1, MinLayerBytes: 0},
		{MaxDataLayers: 1, MinLayerBytes: 1, TargetLayerBytes: 0},
	} {
		if _, err := PlanLayers(1<<20, lim); err == nil {
			t.Fatalf("limits %+v must error", lim)
		}
	}
}

func TestPlanLayersMaxDataOne(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxDataLayers = 1
	for _, est := range []int64{1 << 20, 1 << 40, 100 << 40} {
		p, err := PlanLayers(est, lim)
		if err != nil {
			t.Fatal(err)
		}
		if p.LayerCount != 1 {
			t.Fatalf("est=%d: LayerCount = %d, want 1", est, p.LayerCount)
		}
	}
}
