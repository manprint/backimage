package archive

import (
	"archive/tar"
	"bytes"
	"testing"
)

// tarWith builds an archive from a list of headers with optional bodies.
func tarWith(t *testing.T, entries ...tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := range entries {
		h := entries[i]
		body := make([]byte, h.Size)
		for j := range body {
			body[j] = 'x'
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}
