package chunk

import (
	"encoding/binary"
	"math/bits"
)

// LayerBoundary decides where an OCI data layer ends.
type LayerBoundary interface {
	// ShouldClose reports whether the layer should end after the given chunk.
	ShouldClose(layerBytes int64, chunkDigest [32]byte) bool
}

type fixedBoundary struct{ size int64 }

// NewFixedBoundary closes layers at a fixed size, preserving the behaviour of
// phases 02--09.
func NewFixedBoundary(size int64) LayerBoundary { return fixedBoundary{size: size} }

func (b fixedBoundary) ShouldClose(layerBytes int64, _ [32]byte) bool {
	return b.size > 0 && layerBytes >= b.size
}

type contentBoundary struct {
	min, max int64
	mask     uint64
}

// NewContentBoundary closes layers based on a digest predicate. The expected
// boundary spacing is target bytes; layers are never smaller than target/4 or
// larger than target*4.
func NewContentBoundary(target, min, max int64) LayerBoundary {
	if target <= 0 {
		target = 64 << 20
	}
	if min <= 0 {
		min = target / 4
	}
	if max <= 0 || max < min {
		max = target * 4
	}
	if max < target { // overflow-safe, conservative hard limit.
		max = target
	}
	avgChunk := DefaultCDCParams().Avg
	ratio := uint64((target + avgChunk - 1) / avgChunk)
	bitsN := bits.Len64(ratio - 1)
	var mask uint64
	if bitsN >= 64 {
		mask = ^uint64(0)
	} else {
		mask = (uint64(1) << bitsN) - 1
	}
	return contentBoundary{min: min, max: max, mask: mask}
}

func (b contentBoundary) ShouldClose(layerBytes int64, digest [32]byte) bool {
	if layerBytes >= b.max {
		return true
	}
	if layerBytes < b.min {
		return false
	}
	return binary.BigEndian.Uint64(digest[:8])&b.mask == 0
}
