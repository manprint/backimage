package crypt

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
)

// ErrBadKeyMaterial is returned by Validate for structural problems.
var ErrBadKeyMaterial = errors.New("invalid key material")

// Reasons a key may not seal again. They are separate errors because the
// caller turns each into a different message: only the first is a normal
// consequence of upgrading.
var (
	// ErrKeyNotAttested is material written before the attestation existed.
	// Nothing inside it says which envelope it sealed with, and the public
	// manifest that claims to know is rewritable by anyone.
	ErrKeyNotAttested = errors.New("key material carries no attestation")
	// ErrKeyEpoch is material attesting a different crypto epoch.
	ErrKeyEpoch = errors.New("key material was made for another envelope version")
	// ErrKeyNonceMode is material attesting another nonce mode.
	ErrKeyNonceMode = errors.New("key material was made for another nonce mode")
	// ErrKeyNotReusable is material whose own policy forbids sealing again.
	ErrKeyNotReusable = errors.New("key material is not reusable")
)

const (
	// schemaVersionLegacy is the material written up to 0.4.0: secrets only,
	// no attestation. Still readable, never reusable.
	schemaVersionLegacy = 1
	// schemaVersion is what this build writes.
	schemaVersion = 2
)

// ReusePolicy is what the key itself says about being used again.
type ReusePolicy string

const (
	// ReuseNever is the default: this material seals one backup.
	ReuseNever ReusePolicy = "never"
	// ReuseConvergentDedup allows a later --dedup run under the same epoch
	// and nonce mode to seal with this material again, which is what makes
	// stored blobs shareable between backups.
	ReuseConvergentDedup ReusePolicy = "convergent-dedup"
)

// KeyMaterial holds the secrets of one backup, and the attestation of the
// crypto epoch they were made for. It must be wiped after use.
//
// The attestation is inside the age-wrapped blob, so it is authenticated by
// the wrapping and cannot be edited without the identity that can open it.
// This is the whole point: whether a key may seal again is a security
// decision, and a security decision taken from a public field of manifest.json
// is taken from something an attacker can rewrite. The public field remains, as
// a hint for planning a run before anything is unwrapped, never as authority.
type KeyMaterial struct {
	SchemaVersion int `json:"schemaVersion"`
	// EnvelopeVersion is the crypt envelope in force when this material was
	// generated, NonceMode the mode it was generated for, Reuse its own
	// policy. All three are empty in legacy material.
	EnvelopeVersion int         `json:"envelopeVersion,omitempty"`
	NonceMode       string      `json:"nonceMode,omitempty"`
	Reuse           ReusePolicy `json:"reuse,omitempty"`

	DEK      []byte `json:"dek"`      // 32 bytes, base64 in JSON
	NonceKey []byte `json:"nonceKey"` // 32 bytes, used only in convergent mode
}

// NewKeyMaterial generates fresh random secrets for a single backup: the
// attestation says random nonces and no reuse.
func NewKeyMaterial() (*KeyMaterial, error) {
	return newKeyMaterial(NonceRandom, ReuseNever)
}

// NewDedupKeyMaterial generates fresh random secrets for a convergent run,
// attested as reusable so a later --dedup backup can share stored blobs.
func NewDedupKeyMaterial() (*KeyMaterial, error) {
	return newKeyMaterial(NonceConvergent, ReuseConvergentDedup)
}

func newKeyMaterial(mode NonceMode, reuse ReusePolicy) (*KeyMaterial, error) {
	km := &KeyMaterial{
		SchemaVersion:   schemaVersion,
		EnvelopeVersion: EnvelopeVersion,
		NonceMode:       NonceModeName(mode),
		Reuse:           reuse,
	}
	km.DEK = make([]byte, 32)
	km.NonceKey = make([]byte, 32)
	if _, err := rand.Read(km.DEK); err != nil {
		return nil, fmt.Errorf("crypto/rand (DEK): %w", err)
	}
	if _, err := rand.Read(km.NonceKey); err != nil {
		km.Wipe()
		return nil, fmt.Errorf("crypto/rand (NonceKey): %w", err)
	}
	return km, nil
}

// NonceModeName is the name a nonce mode carries in the attestation and in
// the manifest. An unknown mode has no name: it must not round-trip.
func NonceModeName(mode NonceMode) string {
	switch mode {
	case NonceRandom:
		return "random"
	case NonceConvergent:
		return "convergent"
	default:
		return ""
	}
}

// Attested reports whether this material says anything about itself.
func (k *KeyMaterial) Attested() bool {
	return k != nil && k.SchemaVersion >= schemaVersion
}

// ReusableFor reports whether this material may seal another backup under the
// given epoch and nonce mode, and says why not when it may not.
//
// The epoch must match exactly, in both directions. Material from an older
// envelope may already have sealed two different byte strings under one nonce
// (the pre-0.2.4 convergent derivation), which is enough to recover the GHASH
// authentication key of that DEK; material from a newer one would be dragged
// back to a derivation it was never meant for. Either way the key is burned
// for this build and a new one is generated: the next backup re-uploads its
// blobs once, and dedup returns to normal after that.
func (k *KeyMaterial) ReusableFor(epoch int, mode NonceMode) error {
	if k == nil {
		return fmt.Errorf("%w: nil material", ErrBadKeyMaterial)
	}
	if !k.Attested() {
		return ErrKeyNotAttested
	}
	if k.EnvelopeVersion != epoch {
		return fmt.Errorf("%w: attests envelope %d, this build writes %d",
			ErrKeyEpoch, k.EnvelopeVersion, epoch)
	}
	if want := NonceModeName(mode); k.NonceMode != want {
		return fmt.Errorf("%w: attests %q, this run needs %q", ErrKeyNonceMode, k.NonceMode, want)
	}
	if k.Reuse != ReuseConvergentDedup {
		return fmt.Errorf("%w: policy is %q", ErrKeyNotReusable, k.Reuse)
	}
	return nil
}

// Wipe overwrites the secrets in place. Safe on a nil receiver.
func (k *KeyMaterial) Wipe() {
	if k == nil {
		return
	}
	zero(k.DEK)
	zero(k.NonceKey)
	runtime.KeepAlive(k)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Validate checks lengths, schema version and, from schema 2 on, that the
// attestation is complete. Schema 1 stays valid: a key file written by an
// earlier release must keep opening its own backup.
func (k *KeyMaterial) Validate() error {
	if k == nil {
		return fmt.Errorf("%w: nil material", ErrBadKeyMaterial)
	}
	switch k.SchemaVersion {
	case schemaVersionLegacy:
	case schemaVersion:
		if k.EnvelopeVersion <= 0 {
			return fmt.Errorf("%w: schema %d without an envelope version", ErrBadKeyMaterial, k.SchemaVersion)
		}
		if k.NonceMode != "random" && k.NonceMode != "convergent" {
			return fmt.Errorf("%w: unknown nonce mode %q", ErrBadKeyMaterial, k.NonceMode)
		}
		if k.Reuse != ReuseNever && k.Reuse != ReuseConvergentDedup {
			return fmt.Errorf("%w: unknown reuse policy %q", ErrBadKeyMaterial, k.Reuse)
		}
	default:
		return fmt.Errorf("%w: schema %d (want %d or %d)", ErrBadKeyMaterial, k.SchemaVersion, schemaVersionLegacy, schemaVersion)
	}
	if len(k.DEK) != 32 {
		return fmt.Errorf("%w: DEK is %d bytes, want 32", ErrBadKeyMaterial, len(k.DEK))
	}
	if len(k.NonceKey) != 32 {
		return fmt.Errorf("%w: NonceKey is %d bytes, want 32", ErrBadKeyMaterial, len(k.NonceKey))
	}
	return nil
}

// String never prints secrets.
func (k *KeyMaterial) String() string { return "crypt.KeyMaterial{REDACTED}" }

// GoString never prints secrets.
func (k *KeyMaterial) GoString() string { return "crypt.KeyMaterial{REDACTED}" }

// Clone returns a deep copy; the caller owns the copy.
func (k *KeyMaterial) Clone() *KeyMaterial {
	return &KeyMaterial{
		SchemaVersion:   k.SchemaVersion,
		EnvelopeVersion: k.EnvelopeVersion,
		NonceMode:       k.NonceMode,
		Reuse:           k.Reuse,
		DEK:             append([]byte(nil), k.DEK...),
		NonceKey:        append([]byte(nil), k.NonceKey...),
	}
}

// keyMaterialJSON is the wire shape of the wrapped blob. The attestation
// fields are omitted when empty, so schema 1 material serialises exactly as
// it always did.
type keyMaterialJSON struct {
	SchemaVersion   int         `json:"schemaVersion"`
	EnvelopeVersion int         `json:"envelopeVersion,omitempty"`
	NonceMode       string      `json:"nonceMode,omitempty"`
	Reuse           ReusePolicy `json:"reuse,omitempty"`
	DEK             []byte      `json:"dek"`
	NonceKey        []byte      `json:"nonceKey"`
}

// MarshalJSON serialises the secrets and the attestation (for WrapKeys).
func (k *KeyMaterial) MarshalJSON() ([]byte, error) {
	return json.Marshal(keyMaterialJSON{
		SchemaVersion:   k.SchemaVersion,
		EnvelopeVersion: k.EnvelopeVersion,
		NonceMode:       k.NonceMode,
		Reuse:           k.Reuse,
		DEK:             k.DEK,
		NonceKey:        k.NonceKey,
	})
}

// UnmarshalJSON parses what MarshalJSON produced, and what every earlier
// release produced.
func (k *KeyMaterial) UnmarshalJSON(b []byte) error {
	var a keyMaterialJSON
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	k.SchemaVersion = a.SchemaVersion
	k.EnvelopeVersion = a.EnvelopeVersion
	k.NonceMode = a.NonceMode
	k.Reuse = a.Reuse
	k.DEK = a.DEK
	k.NonceKey = a.NonceKey
	return nil
}
