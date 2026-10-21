//go:build unix

package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestTruncatedPayloadLeavesNoPartialFile is the regression for a write that
// fails half way (a truncated stream, ENOSPC, EIO): the file stayed in the
// destination under its real name with its tail missing, indistinguishable
// from a restored one.
func TestTruncatedPayloadLeavesNoPartialFile(t *testing.T) {
	full := tarWith(t, tar.Header{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4096})
	cut := bytes.NewReader(full.Bytes()[:512+1000]) // header + part of the body
	dst := t.TempDir()
	if _, err := extractorFor(ExtractOptions{}).Extract(context.Background(), cut, dst); err == nil {
		t.Fatal("a truncated payload was extracted without error")
	}
	if _, err := os.Lstat(filepath.Join(dst, "big")); !os.IsNotExist(err) {
		t.Fatalf("partial file left in the destination: %v", err)
	}
}
