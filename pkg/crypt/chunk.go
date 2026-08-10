package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/manprint/backimage/pkg/compress"
)

// NonceMode selects how the per-chunk nonce is derived.
type NonceMode uint8

const (
	// NonceRandom draws 12 random bytes per chunk. Default.
	NonceRandom NonceMode = 0
	// NonceConvergent derives the nonce from the plaintext digest, enabling
	// deduplication at the cost of revealing chunk equality. Phase 10.
	NonceConvergent NonceMode = 1
)

// ErrIntegrity is returned when authentication fails. It maps to exit code 5.
var ErrIntegrity = errors.New("blob authentication failed")

// Sealer encrypts already-compressed chunk payloads.
type Sealer interface {
	// Seal writes the full stored blob (header + ciphertext) for one chunk.
	// plainSHA is the digest of the *plaintext* chunk, used in convergent mode.
	Seal(dst []byte, chunkIndex uint32, codec compress.Codec, compressed []byte, plainSHA [32]byte) ([]byte, error)
	// Overhead returns the number of bytes Seal adds to the payload.
	Overhead() int
}

// Opener decrypts stored blobs.
type Opener interface {
	// Open returns the compressed payload of one stored blob.
	Open(dst []byte, chunkIndex uint32, blob []byte) ([]byte, compress.ID, error)
}

type sealer struct {
	ae       cipher.AEAD // nil when encryption is disabled
	km       *KeyMaterial
	mode     NonceMode
	overhead int
}

type opener struct {
	ae cipher.AEAD // nil when encryption is disabled
	km *KeyMaterial
}

// NewSealer builds a Sealer. When km is nil, encryption is disabled and the
// envelope is written with aead=0 (the payload stays in clear).
func NewSealer(km *KeyMaterial, mode NonceMode) (Sealer, error) {
	if km == nil {
		return &sealer{overhead: headerEndSize}, nil
	}
	if err := km.Validate(); err != nil {
		return nil, err
	}
	if mode != NonceRandom && mode != NonceConvergent {
		return nil, fmt.Errorf("unknown nonce mode %d", mode)
	}
	ae, err := gcmFor(km.DEK)
	if err != nil {
		return nil, err
	}
	return &sealer{ae: ae, km: km, mode: mode, overhead: headerMaxSize + 16}, nil
}

// NewOpener builds an Opener. km may be nil only for unencrypted blobs.
func NewOpener(km *KeyMaterial) (Opener, error) {
	if km == nil {
		return &opener{}, nil
	}
	if err := km.Validate(); err != nil {
		return nil, err
	}
	ae, err := gcmFor(km.DEK)
	if err != nil {
		return nil, err
	}
	return &opener{ae: ae, km: km}, nil
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Overhead returns the number of bytes Seal adds to the payload.
func (s *sealer) Overhead() int { return s.overhead }

// Seal writes the full envelope for one chunk as an extension of dst.
// dst must have capacity for the extra bytes to keep the call allocation-free.
func (s *sealer) Seal(dst []byte, chunkIndex uint32, codec compress.Codec, compressed []byte, plainSHA [32]byte) ([]byte, error) {
	start := len(dst)
	if s.ae == nil {
		// Encryption off: header without nonce, plaintext payload.
		dst = append(dst, make([]byte, HeaderSize(aeadNone))...)
		if _, err := MarshalHeader(dst[start:], Header{
			Version: envelopeVersion,
			Codec:   codec.ID(),
			AEAD:    aeadNone,
		}); err != nil {
			return dst, err
		}
		return append(dst, compressed...), nil
	}

	h := Header{
		Version: envelopeVersion,
		Codec:   codec.ID(),
		AEAD:    aeadAES256GCM,
	}
	if s.mode == NonceConvergent {
		h.Flags |= flagConvergent
	}
	var nonce [nonceLen]byte
	switch s.mode {
	case NonceRandom:
		if _, err := rand.Read(nonce[:]); err != nil {
			return dst, fmt.Errorf("crypto/rand (nonce): %w", err)
		}
	case NonceConvergent:
		mac := hmac.New(sha256.New, s.km.NonceKey)
		mac.Write(plainSHA[:])
		copy(nonce[:], mac.Sum(nil))
	}
	h.Nonce = nonce

	dst = append(dst, make([]byte, headerMaxSize)...)
	if _, err := MarshalHeader(dst[start:], h); err != nil {
		return dst, err
	}
	dst = s.ae.Seal(dst, nonce[:], compressed, AAD(h, chunkIndex))
	return dst, nil
}

// Open returns the compressed payload of one stored blob.
func (o *opener) Open(dst []byte, chunkIndex uint32, blob []byte) ([]byte, compress.ID, error) {
	h, n, err := ParseHeader(blob)
	if err != nil {
		return dst, 0, err
	}
	switch h.AEAD {
	case aeadNone:
		// Clear blob; keyed or keyless opener both may read it.
		return append(dst, blob[n:]...), h.Codec, nil
	case aeadAES256GCM:
		if o.ae == nil {
			return dst, 0, errors.New("encrypted blob: key material required")
		}
		out, err := o.ae.Open(dst, h.Nonce[:], blob[n:], AAD(h, chunkIndex))
		if err != nil {
			return dst, 0, ErrIntegrity
		}
		return out, h.Codec, nil
	default:
		return dst, 0, fmt.Errorf("unsupported aead %d", h.AEAD)
	}
}
