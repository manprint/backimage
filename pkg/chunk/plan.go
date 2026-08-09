package chunk

import (
	"fmt"
)

// LayerLimits encodes the constraints of a runnable OCI image.
type LayerLimits struct {
	MaxDataLayers    int   // 118 by default: 127 overlayfs limit minus binary+metadata+margin
	MaxLayerBytes    int64 // hard registry-side ceiling, default 5 GiB (warning above)
	MinLayerBytes    int64 // default 16 MiB
	TargetLayerBytes int64 // from --max-layer-size, default 1 GiB
}

// DefaultLimits returns the limits used when the user gives no override.
func DefaultLimits() LayerLimits {
	return LayerLimits{
		MaxDataLayers:    118,
		MaxLayerBytes:    5 << 30,
		MinLayerBytes:    16 << 20,
		TargetLayerBytes: 1 << 30,
	}
}

// Plan describes how chunks map onto image layers.
type Plan struct {
	LayerBytes int64
	LayerCount int
	ChunkBytes int64
	Warnings   []string
}

// PlanLayers computes a layout for a stream of the given estimated size. It
// never exceeds limits.MaxDataLayers: when the estimate is large it grows
// LayerBytes instead of LayerCount (decision D05).
func PlanLayers(estimatedStoredBytes int64, limits LayerLimits) (Plan, error) {
	if limits.MaxDataLayers < 1 {
		return Plan{}, fmt.Errorf("plan: MaxDataLayers must be >= 1, got %d", limits.MaxDataLayers)
	}
	if limits.MinLayerBytes < 1 {
		return Plan{}, fmt.Errorf("plan: MinLayerBytes must be >= 1, got %d", limits.MinLayerBytes)
	}
	if limits.TargetLayerBytes < 1 {
		return Plan{}, fmt.Errorf("plan: TargetLayerBytes must be >= 1, got %d", limits.TargetLayerBytes)
	}
	p := Plan{LayerBytes: limits.MinLayerBytes, LayerCount: 1}
	if estimatedStoredBytes <= 0 {
		p.ChunkBytes = clampChunkBytes(p.LayerBytes)
		return p, nil
	}
	// Step 2: start from the user target size.
	layerBytes := limits.TargetLayerBytes
	// Step 3: count layers without exceeding the overlayfs ceiling.
	layerCount := ceilDiv(estimatedStoredBytes, layerBytes)
	// Step 4: too many layers -> grow the layer instead (D05).
	if layerCount > limits.MaxDataLayers {
		layerCount = limits.MaxDataLayers
		layerBytes = int64(ceilDiv(estimatedStoredBytes, int64(layerCount)))
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"backup grande: dimensione layer portata a %d per restare entro %d layer (limite overlayfs)",
			layerBytes, limits.MaxDataLayers))
	}
	// Step 5: tiny inputs must still fill a minimal layer.
	if layerBytes < limits.MinLayerBytes {
		layerBytes = limits.MinLayerBytes
		layerCount = ceilDiv(estimatedStoredBytes, layerBytes)
	}
	// A single layer should size itself to the input, never the target:
	// "100 MiB, target 1 GiB" -> one 100 MiB layer; "10 MiB" -> MinLayerBytes.
	if layerCount == 1 {
		if layerBytes > estimatedStoredBytes {
			layerBytes = estimatedStoredBytes
		}
		if layerBytes < limits.MinLayerBytes {
			layerBytes = limits.MinLayerBytes
		}
	}
	// Step 6: registry ceiling is a warning, not an error.
	if layerBytes > limits.MaxLayerBytes {
		p.Warnings = append(p.Warnings, fmt.Sprintf("layer da %d: alcuni registry rifiutano blob cosi' grandi", layerBytes))
	}
	p.LayerBytes = layerBytes
	p.LayerCount = layerCount
	p.ChunkBytes = clampChunkBytes(layerBytes)
	return p, nil
}

func clampChunkBytes(layerBytes int64) int64 {
	div := layerBytes / 64
	if div < MinChunkSize {
		div = MinChunkSize
	}
	if div > 64<<20 {
		div = 64 << 20
	}
	return div
}

func ceilDiv(a, b int64) int {
	return int((a + b - 1) / b)
}
