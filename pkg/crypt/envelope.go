package crypt

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/manprint/backimage/pkg/compress"
)

// Envelope layout (overview.md §4.4):
//
//	0  8  magic  "BIMGCHK1"
//	8  1  version (1 = legacy, 2 = role-bound AAD, payload-derived nonce)
//	9  1  codec   (compress.ID)
//	10 1  aead    (0=none, 1=aes-256-gcm)
//	11 1  flags   (bit0 = convergent nonce)
//	12 12 nonce   (present only if aead != 0)
//	24 .. payload (compressed, then encrypted) + GCM tag 16 B
//
// The byte layout is identical in both versions: only the meaning of the
// authenticated data and the derivation of a convergent nonce changed.
const (
	envelopeMagic = "BIMGCHK1"
	// envelopeVersionLegacy is what backimage wrote up to 0.2.3: an AAD that
	// does not name the role of the blob, and a convergent nonce derived from
	// the digest of the *plaintext* chunk rather than from the bytes AES-GCM
	// actually encrypts. It is still read, so existing backups keep restoring,
	// and never written.
	envelopeVersionLegacy = 1
	// envelopeVersion is the version Seal writes.
	envelopeVersion = 2
	aeadNone        = 0
	aeadAES256GCM   = 1
	flagConvergent  = 1 << 0
	nonceLen        = 12
	headerEndSize   = 12 // magic+version+codec+aead+flags
	headerNonceSize = 12
	headerMaxSize   = headerEndSize + headerNonceSize // 24
)

// EnvelopeVersion is the envelope version this build writes. The manifest
// records it so a later backup can tell whether the key it is about to reuse
// ever sealed a blob with the legacy derivation.
const EnvelopeVersion = envelopeVersion

// Role names what a sealed blob is. Version 2 authenticates it, so the file
// index, the confidential metadata and a data chunk are not interchangeable
// even when one key seals all three.
type Role uint8

const (
	// RoleData is one data chunk of the archive stream.
	RoleData Role = 0
	// RoleIndex is index.json.zst, the per-file table.
	RoleIndex Role = 1
	// RolePrivate is private.json.zst, the confidential metadata.
	RolePrivate Role = 2
)

func (r Role) valid() bool { return r <= RolePrivate }

// String names the role for error messages.
func (r Role) String() string {
	switch r {
	case RoleData:
		return "data"
	case RoleIndex:
		return "index"
	case RolePrivate:
		return "private"
	default:
		return fmt.Sprintf("role(%d)", uint8(r))
	}
}

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
	if h.Version != envelopeVersion && h.Version != envelopeVersionLegacy {
		return Header{}, 0, fmt.Errorf("unsupported blob version %d (support %d-%d)",
			h.Version, envelopeVersionLegacy, envelopeVersion)
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

// AAD builds the additional authenticated data for one blob.
//
// Version 2 authenticates the role, so a data chunk cannot be accepted where
// the file index or the private metadata is expected. Before that the three
// were sealed with an identical AAD at chunk index 0, which made them
// interchangeable under one key.
//
// Random-nonce blobs also bind their position, to detect a reordered chunk
// table. Convergent blobs deliberately omit the position: a CDC boundary may
// move a matching chunk to another index in a later backup, and binding that
// index would make otherwise identical encrypted blobs different. Their
// integrity across positions is carried by the per-chunk plaintext digests in
// the sealed private blob, which restore always verifies.
//
// The version 1 layout is reproduced byte for byte, or backups written before
// 0.2.4 would stop opening.
func AAD(h Header, role Role, chunkIndex uint32) []byte {
	if h.Version == envelopeVersionLegacy {
		out := make([]byte, 16)
		copy(out[0:8], envelopeMagic)
		out[8] = h.Version
		out[9] = byte(h.Codec)
		out[10] = h.AEAD
		out[11] = h.Flags
		if h.Flags&flagConvergent == 0 {
			binary.BigEndian.PutUint32(out[12:16], chunkIndex)
		}
		return out
	}
	out := make([]byte, 17)
	copy(out[0:8], envelopeMagic)
	out[8] = h.Version
	out[9] = byte(h.Codec)
	out[10] = h.AEAD
	out[11] = h.Flags
	out[12] = byte(role)
	if h.Flags&flagConvergent == 0 {
		binary.BigEndian.PutUint32(out[13:17], chunkIndex)
	}
	return out
}
