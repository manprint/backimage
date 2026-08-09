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

const schemaVersion = 1

// KeyMaterial holds the secrets of one backup. It must be wiped after use.
type KeyMaterial struct {
	SchemaVersion int    `json:"schemaVersion"`
	DEK           []byte `json:"dek"`      // 32 bytes, base64 in JSON
	NonceKey      []byte `json:"nonceKey"` // 32 bytes, used only in convergent mode
}

// NewKeyMaterial generates fresh random secrets using crypto/rand.
func NewKeyMaterial() (*KeyMaterial, error) {
	km := &KeyMaterial{SchemaVersion: schemaVersion}
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

// Validate checks lengths and schema version.
func (k *KeyMaterial) Validate() error {
	if k == nil {
		return fmt.Errorf("%w: nil material", ErrBadKeyMaterial)
	}
	if k.SchemaVersion != schemaVersion {
		return fmt.Errorf("%w: schema %d (want %d)", ErrBadKeyMaterial, k.SchemaVersion, schemaVersion)
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
		SchemaVersion: k.SchemaVersion,
		DEK:           append([]byte(nil), k.DEK...),
		NonceKey:      append([]byte(nil), k.NonceKey...),
	}
}

// MarshalJSON serialises the secrets (for WrapKeys).
func (k *KeyMaterial) MarshalJSON() ([]byte, error) {
	type alias struct {
		SchemaVersion int    `json:"schemaVersion"`
		DEK           []byte `json:"dek"`
		NonceKey      []byte `json:"nonceKey"`
	}
	return json.Marshal(alias{SchemaVersion: k.SchemaVersion, DEK: k.DEK, NonceKey: k.NonceKey})
}

// UnmarshalJSON parses secrets produced by MarshalJSON.
func (k *KeyMaterial) UnmarshalJSON(b []byte) error {
	var a struct {
		SchemaVersion int    `json:"schemaVersion"`
		DEK           []byte `json:"dek"`
		NonceKey      []byte `json:"nonceKey"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	k.SchemaVersion = a.SchemaVersion
	k.DEK = a.DEK
	k.NonceKey = a.NonceKey
	return nil
}
