package compress

import (
	"bytes"
	"testing"
)

func FuzzRoundTripZstd(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("hello world"))
	f.Add(bytes.Repeat([]byte{0x00}, 4096))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Get("zstd")
		if err != nil {
			t.Fatal(err)
		}
		out := unarchive(t, c, archive(t, c, 2, data))
		if !bytes.Equal(out, data) {
			t.Fatalf("round-trip mismatch: %d bytes in, %d out", len(data), len(out))
		}
	})
}

func FuzzRoundTripGzip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("hello world"))
	f.Add(bytes.Repeat([]byte{0xff}, 1024))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Get("gzip")
		if err != nil {
			t.Fatal(err)
		}
		out := unarchive(t, c, archive(t, c, 6, data))
		if !bytes.Equal(out, data) {
			t.Fatalf("round-trip mismatch: %d bytes in, %d out", len(data), len(out))
		}
	})
}
