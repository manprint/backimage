package crypt

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"

	"github.com/fpierri/backimage/pkg/compress"
)

func TestHeaderRoundTrip(t *testing.T) {
	for _, codec := range []compress.ID{compress.Store, compress.Gzip, compress.Zstd, compress.Xz, compress.Lz4} {
		for _, aead := range []uint8{aeadNone, aeadAES256GCM} {
			for _, flags := range []uint8{0, flagConvergent} {
				h := Header{
					Version: envelopeVersion,
					Codec:   codec,
					AEAD:    aead,
					Flags:   flags,
				}
				if aead != aeadNone {
					for i := range h.Nonce {
						h.Nonce[i] = byte(i + 1)
					}
				}
				dst := make([]byte, HeaderSize(aead))
				n, err := MarshalHeader(dst, h)
				if err != nil {
					t.Fatal(err)
				}
				if n != HeaderSize(aead) {
					t.Fatalf("marshal wrote %d bytes, want %d", n, HeaderSize(aead))
				}
				back, m, err := ParseHeader(dst)
				if err != nil {
					t.Fatal(err)
				}
				if m != n {
					t.Fatalf("parse consumed %d bytes, want %d", m, n)
				}
				if back != h {
					t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", back, h)
				}
			}
		}
	}
}

func TestParseHeaderGarbage(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		buf := make([]byte, 40)
		r.Read(buf)
		if i%2 == 0 {
			buf = buf[:24]
		}
		if _, _, err := ParseHeader(buf); err == nil {
			t.Fatalf("random blob parsed clean: %x", buf)
		}
	}
}

func TestParseHeaderBadMagic(t *testing.T) {
	buf := bytes.Repeat([]byte("X"), 24)
	_, _, err := ParseHeader(buf)
	if err == nil || err.Error() != "not a backimage blob" {
		t.Fatalf("want 'not a backimage blob', got %v", err)
	}
}

func TestParseHeaderBadVersion(t *testing.T) {
	dst := make([]byte, 12)
	MarshalHeader(dst, Header{Version: envelopeVersion, Codec: compress.Store, AEAD: aeadNone})
	dst[8] = 9
	_, _, err := ParseHeader(dst)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version error, got %v", err)
	}
}

func TestParseHeaderBadCodec(t *testing.T) {
	dst := make([]byte, 12)
	MarshalHeader(dst, Header{Version: envelopeVersion, Codec: compress.Store, AEAD: aeadNone})
	dst[9] = 200
	if _, _, err := ParseHeader(dst); err == nil {
		t.Fatal("unknown codec must error")
	}
}

func TestParseHeaderBadAEAD(t *testing.T) {
	dst := make([]byte, 12)
	MarshalHeader(dst, Header{Version: envelopeVersion, Codec: compress.Store, AEAD: aeadNone})
	dst[10] = 7
	if _, _, err := ParseHeader(dst); err == nil {
		t.Fatal("unknown aead must error")
	}
}

func TestAADBindsChunkIndex(t *testing.T) {
	h := Header{Version: envelopeVersion, Codec: compress.Zstd, AEAD: aeadAES256GCM, Flags: 0}
	if bytes.Equal(AAD(h, 0), AAD(h, 1)) {
		t.Fatal("AAD must differ per chunk index")
	}
	if bytes.Equal(AAD(h, 42), AAD(h, 43)) {
		t.Fatal("AAD must differ per chunk index")
	}
	h2 := h
	h2.Flags = flagConvergent
	if bytes.Equal(AAD(h, 0), AAD(h2, 0)) {
		t.Fatal("AAD must bind flags")
	}
	if !bytes.Equal(AAD(h2, 0), AAD(h2, 1)) {
		t.Fatal("convergent AAD must not bind the movable chunk index")
	}
}

func TestMarshalHeaderTooSmall(t *testing.T) {
	if _, err := MarshalHeader(make([]byte, 11), Header{AEAD: aeadAES256GCM}); err == nil {
		t.Fatal("short dst must error")
	}
	if _, err := MarshalHeader(make([]byte, 11), Header{AEAD: aeadNone}); err == nil {
		t.Fatal("short dst must error")
	}
}

func FuzzParseHeader(f *testing.F) {
	f.Add([]byte("BIMGCHK1\x01\x02\x00\x00"))
	f.Add([]byte("garbage"))
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte("BIMGCHK1\x01\xff\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, n, err := ParseHeader(data)
		if err != nil {
			return
		}
		if n < headerEndSize || n > len(data) {
			t.Fatalf("bad consumed size %d for len %d", n, len(data))
		}
		if h.Version != envelopeVersion {
			t.Fatalf("freshly parsed header has unsupported version %d", h.Version)
		}
		if h.AEAD != aeadNone && n != 24 {
			t.Fatalf("aead %d must imply header size 24, got %d", h.AEAD, n)
		}
	})
}
