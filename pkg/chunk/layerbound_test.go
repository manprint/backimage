package chunk

import (
	"crypto/sha256"
	"testing"
)

func TestFixedBoundary(t *testing.T) {
	b := NewFixedBoundary(10)
	if b.ShouldClose(9, [32]byte{}) || !b.ShouldClose(10, [32]byte{}) {
		t.Fatal("fixed boundary threshold changed")
	}
	if NewFixedBoundary(0).ShouldClose(100, [32]byte{}) {
		t.Fatal("zero fixed boundary must be disabled")
	}
}

func TestContentBoundaryLimitsAndDeterminism(t *testing.T) {
	b := NewContentBoundary(64<<20, 16<<20, 256<<20)
	d := sha256.Sum256([]byte("stable chunk"))
	if b.ShouldClose(16<<20-1, d) {
		t.Fatal("content boundary closed below min")
	}
	if !b.ShouldClose(256<<20, d) {
		t.Fatal("content boundary ignored hard max")
	}
}

func TestContentBoundaryFindsDigestAtTarget(t *testing.T) {
	b := NewContentBoundary(64<<20, 16<<20, 256<<20)
	var found [32]byte
	for i := 0; i < 1<<16; i++ {
		d := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		if b.ShouldClose(64<<20, d) {
			found = d
			break
		}
	}
	if found == ([32]byte{}) || !b.ShouldClose(64<<20, found) {
		t.Fatal("digest predicate did not find a boundary")
	}
}
