//go:build !embedded

package embedded

import (
	"errors"
	"strings"
	"testing"
)

func TestSelfExtractPlaceholderOrReal(t *testing.T) {
	data, err := SelfExtract("amd64")
	if err != nil {
		if !errors.Is(err, ErrNotEmbedded) {
			t.Fatalf("unexpected error for amd64: %v", err)
		}
		return // placeholder build: expected
	}
	// Real build (after make selfextract): must be a meaningful binary.
	if len(data) < 1<<20 {
		t.Fatalf("real payload too small: %d bytes", len(data))
	}
}

func TestSelfExtractUnknownArch(t *testing.T) {
	_, err := SelfExtract("mips")
	if err == nil {
		t.Fatal("expected error for unknown arch")
	}
	if !strings.Contains(err.Error(), "mips") {
		t.Fatalf("error must name the arch: %v", err)
	}
}

func TestArchitectures(t *testing.T) {
	arches := Architectures()
	if len(arches) != 2 || arches[0] != "amd64" || arches[1] != "arm64" {
		t.Fatalf("unexpected arches: %v", arches)
	}
}
