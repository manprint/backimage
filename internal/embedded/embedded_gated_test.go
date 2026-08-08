//go:build embedded

package embedded

import (
	"bytes"
	"testing"
)

// This test runs only after `make selfextract` has replaced the placeholders:
//
//	go test -tags embedded ./internal/embedded
func TestRealBinariesAreELF(t *testing.T) {
	for _, arch := range Architectures() {
		data, err := SelfExtract(arch)
		if err != nil {
			t.Fatalf("SelfExtract(%s): %v", arch, err)
		}
		if len(data) < 1<<20 {
			t.Errorf("SelfExtract(%s): %d bytes, want > 1 MiB", arch, len(data))
		}
		if !bytes.HasPrefix(data, []byte("\x7fELF")) {
			t.Errorf("SelfExtract(%s): not an ELF binary", arch)
		}
	}
}
