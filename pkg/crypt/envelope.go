package crypt

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/fpierri/backimage/pkg/compress"
)

// Envelope layout (overview.md §4.4):
//
//	0  8  magic  "BIMGCHK1"
//	8  1  version = 1
//	9  1  codec   (compress.ID)
//	10 1  aead    (0=none, 1=aes-256-gcm)
//	11 1  flags   (bit0 = convergent nonce)
//	12 12 nonce   (present only if aead != 0)
//	24 .. payload (compressed, then encrypted) + GCM tag 16 B
const (
	envelopeMagic   = "BIMGCHK1"
	envelopeVersion = 1
	aeadNone        = 0
	aeadAES256GCM   = 1
	flagConvergent  = 1 << 0
	nonceLen        = 12
	headerEndSize   = 12 // magic+version+codec+aead+flags
	headerNonceSize = 12
	headerMaxSize   = headerEndSize + headerNonceSize // 24
)

// Header is the fixed prefix of every stored blob.
type Header struct {
	Version uint8
	Codec   compress.ID
	AEAD    uint8
	Flags   uint8
	Nonce   [nonceLen]byte
}

// HeaderSize returns the encoded size for the given AEAD setting.
func HeaderSize(aead uint8) int {
	if aead == aeadNone {
		return headerEndSize
	}
	return headerMaxSize
}

// MarshalHeader encodes h into dst, which must be at least HeaderSize bytes.
// It never allocates.
func MarshalHeader(dst []byte, h Header) (int, error) {
	if len(dst) < headerEndSize {
		return 0, errors.New("crypt: dst too small for header")
	}
	n := headerEndSize
	if h.AEAD == aeadAES256GCM {
		if len(dst) < headerMaxSize {
			return 0, errors.New("crypt: dst too small for header nonce")
		}
		n = headerMaxSize
		copy(dst[headerEndSize:headerMaxSize], h.Nonce[:])
	}
	copy(dst, envelopeMagic)
	dst[8] = h.Version
	dst[9] = byte(h.Codec)
	dst[10] = h.AEAD
	dst[11] = h.Flags
	return n, nil
}

// IsEnvelope reports whether b starts with the backimage blob magic.
func IsEnvelope(b []byte) bool {
	return len(b) >= len(envelopeMagic) && string(b[:len(envelopeMagic)]) == envelopeMagic
}

// ParseHeader decodes a header from src.
func ParseHeader(src []byte) (Header, int, error) {
	if len(src) < headerEndSize {
		return Header{}, 0, fmt.Errorf("short blob: %d bytes", len(src))
	}
	if string(src[:8]) != envelopeMagic {
		return Header{}, 0, errors.New("not a backimage blob")
	}
	h := Header{
		Version: src[8],
		Codec:   compress.ID(src[9]),
		AEAD:    src[10],
		Flags:   src[11],
	}
	if h.Version != envelopeVersion {
		return Header{}, 0, fmt.Errorf("unsupported blob version %d (support %d)", h.Version, envelopeVersion)
	}
	if _, err := compress.ByID(h.Codec); err != nil {
		return Header{}, 0, fmt.Errorf("unknown codec %d", h.Codec)
	}
	if h.AEAD != aeadNone && h.AEAD != aeadAES256GCM {
		return Header{}, 0, fmt.Errorf("unknown aead %d", h.AEAD)
	}
	n := HeaderSize(h.AEAD)
	if len(src) < n {
		return Header{}, 0, fmt.Errorf("short blob header: need %d bytes, have %d", n, len(src))
	}
	if h.AEAD != aeadNone {
		copy(h.Nonce[:], src[headerEndSize:n])
	}
	return h, n, nil
}

// AAD builds the additional authenticated data for chunk index i.
func AAD(h Header, chunkIndex uint32) []byte {
	out := make([]byte, 16)
	copy(out[0:8], envelopeMagic)
	out[8] = h.Version
	out[9] = byte(h.Codec)
	out[10] = h.AEAD
	out[11] = h.Flags
	binary.BigEndian.PutUint32(out[12:16], chunkIndex)
	return out
}
