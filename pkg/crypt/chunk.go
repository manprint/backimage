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
	// NonceConvergent derives the nonce from the sealed payload, enabling
	// deduplication at the cost of revealing chunk equality. Phase 10.
	NonceConvergent NonceMode = 1
)

// nonceLabel domain-separates the convergent nonce derivation. The "v2" is the
// derivation, not the tool version: it changed in 0.2.4 together with the
// envelope version.
const nonceLabel = "backimage/nonce/v2\x00"

// ErrIntegrity is returned when authentication fails. It maps to exit code 5.
var ErrIntegrity = errors.New("blob authentication failed")

// Sealer encrypts already-compressed payloads.
type Sealer interface {
	// Seal writes the full stored blob (header + ciphertext) for one payload.
	// role names what the blob is and is authenticated; chunkIndex binds the
	// position of a data chunk in random-nonce mode. payload is exactly what
	// gets encrypted: nothing outside it may influence the nonce.
	Seal(dst []byte, role Role, chunkIndex uint32, codec compress.Codec, payload []byte) ([]byte, error)
	// Overhead returns the number of bytes Seal adds to the payload.
	Overhead() int
}

// Opener decrypts stored blobs.
//
// There are two implementations and the difference between them is the whole
// point: a keyed opener refuses anything that is not authenticated, a clear
// opener refuses anything that is. Which one a reader holds is decided once,
// from what the manifest says the backup is, and never from what a blob header
// claims to be — a blob header is written by whoever wrote the blob.
type Opener interface {
	// Open returns the compressed payload of one stored blob. role must be the
	// one the blob was sealed with.
	Open(dst []byte, role Role, chunkIndex uint32, blob []byte) ([]byte, compress.ID, error)
	// RequiresAuthentication reports whether this opener rejects blobs that
	// carry no AEAD tag. Callers use it to state their expectation about a
	// whole blob before parsing it, not to choose a policy after seeing one.
	RequiresAuthentication() bool
}

type sealer struct {
	ae       cipher.AEAD // nil when encryption is disabled
	km       *KeyMaterial
	mode     NonceMode
	overhead int
}

// keyedOpener reads the blobs of an encrypted backup. Every blob must carry a
// valid AES-256-GCM tag; an unauthenticated one is an integrity failure, not a
// blob to be read faster.
type keyedOpener struct {
	ae cipher.AEAD
	km *KeyMaterial
}

// clearOpener reads the blobs of a backup declared unencrypted. It has no key
// and cannot acquire one, so an encrypted blob is an error rather than a
// prompt.
type clearOpener struct{}

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

// NewKeyedOpener builds the Opener of an encrypted backup. km is mandatory.
//
// It exists as a separate constructor because the single permissive opener it
// replaces returned the payload of an unauthenticated blob even while holding
// the key: an attacker who could rewrite a blob could strip its AEAD tag,
// declare aead=none in the header, and have the reader hand the bytes over
// unverified. Splitting the constructors makes that downgrade unrepresentable
// rather than merely unlikely — there is no longer an object that can do both.
func NewKeyedOpener(km *KeyMaterial) (Opener, error) {
	if km == nil {
		return nil, errors.New("keyed opener requires key material")
	}
	if err := km.Validate(); err != nil {
		return nil, err
	}
	ae, err := gcmFor(km.DEK)
	if err != nil {
		return nil, err
	}
	return &keyedOpener{ae: ae, km: km}, nil
}

// NewClearOpener builds the Opener of a backup declared unencrypted.
func NewClearOpener() Opener { return clearOpener{} }

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Overhead returns the number of bytes Seal adds to the payload.
func (s *sealer) Overhead() int { return s.overhead }

// convergentNonce derives the nonce of a convergent blob from the exact bytes
// AES-GCM is about to encrypt.
//
// Deriving it from anything else is a nonce-reuse bug waiting to happen. Up to
// 0.2.3 the nonce came from the digest of the *plaintext* chunk while GCM
// encrypted the *compressed* chunk, so two backups sharing a repository key
// sealed two different byte strings under one nonce as soon as the compressed
// form of a chunk changed: a different --compression or --compression-level
// between two runs does it, and so does a compressor release that reshapes its
// output at the same level. Two AES-GCM messages under one key and nonce hand
// an attacker the XOR of their plaintexts and the GHASH authentication key,
// which is forgery of arbitrary authenticated blobs under that DEK.
//
// Keying the nonce on the sealed bytes makes that impossible by construction:
// equal nonce now means equal payload, which is exactly the case deduplication
// wants, so nothing is lost.
func convergentNonce(nonceKey []byte, role Role, payload []byte) []byte {
	sum := sha256.Sum256(payload)
	mac := hmac.New(sha256.New, nonceKey)
	mac.Write([]byte(nonceLabel))
	mac.Write([]byte{byte(role)})
	mac.Write(sum[:])
	return mac.Sum(nil)[:nonceLen]
}

// Seal writes the full envelope for one payload as an extension of dst.
// dst must have capacity for the extra bytes to keep the call allocation-free.
func (s *sealer) Seal(dst []byte, role Role, chunkIndex uint32, codec compress.Codec, payload []byte) ([]byte, error) {
	if !role.valid() {
		return dst, fmt.Errorf("unknown blob role %d", uint8(role))
	}
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
		return append(dst, payload...), nil
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
		copy(nonce[:], convergentNonce(s.km.NonceKey, role, payload))
	}
	h.Nonce = nonce

	dst = append(dst, make([]byte, headerMaxSize)...)
	if _, err := MarshalHeader(dst[start:], h); err != nil {
		return dst, err
	}
	dst = s.ae.Seal(dst, nonce[:], payload, AAD(h, role, chunkIndex))
	return dst, nil
}

// RequiresAuthentication reports that this opener rejects unauthenticated blobs.
func (o *keyedOpener) RequiresAuthentication() bool { return true }

// Open returns the compressed payload of one stored blob of an encrypted
// backup. An unauthenticated blob is refused: inside a backup that declares
// itself encrypted there is no legitimate one, and reading it would be exactly
// the downgrade the two-opener split exists to prevent.
func (o *keyedOpener) Open(dst []byte, role Role, chunkIndex uint32, blob []byte) ([]byte, compress.ID, error) {
	if !role.valid() {
		return dst, 0, fmt.Errorf("unknown blob role %d", uint8(role))
	}
	h, n, err := ParseHeader(blob)
	if err != nil {
		return dst, 0, err
	}
	switch h.AEAD {
	case aeadNone:
		return dst, 0, fmt.Errorf("%w: unauthenticated blob in an encrypted backup", ErrIntegrity)
	case aeadAES256GCM:
		out, err := o.ae.Open(dst, h.Nonce[:], blob[n:], AAD(h, role, chunkIndex))
		if err != nil {
			return dst, 0, ErrIntegrity
		}
		return out, h.Codec, nil
	default:
		return dst, 0, fmt.Errorf("unsupported aead %d", h.AEAD)
	}
}

// RequiresAuthentication reports that this opener has no key and therefore
// cannot authenticate anything.
func (clearOpener) RequiresAuthentication() bool { return false }

// Open returns the payload of one blob of a backup declared unencrypted.
func (clearOpener) Open(dst []byte, role Role, _ uint32, blob []byte) ([]byte, compress.ID, error) {
	if !role.valid() {
		return dst, 0, fmt.Errorf("unknown blob role %d", uint8(role))
	}
	h, n, err := ParseHeader(blob)
	if err != nil {
		return dst, 0, err
	}
	switch h.AEAD {
	case aeadNone:
		return append(dst, blob[n:]...), h.Codec, nil
	case aeadAES256GCM:
		return dst, 0, errors.New("encrypted blob: key material required")
	default:
		return dst, 0, fmt.Errorf("unsupported aead %d", h.AEAD)
	}
}
