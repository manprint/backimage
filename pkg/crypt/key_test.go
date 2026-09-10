package crypt

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNewKeyMaterialDistinct(t *testing.T) {
	a, err := NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Wipe()
	b, err := NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Wipe()
	if bytes.Equal(a.DEK, b.DEK) {
		t.Fatal("two DEKs must differ")
	}
	if len(a.DEK) != 32 || len(a.NonceKey) != 32 {
		t.Fatalf("wrong lengths: %d, %d", len(a.DEK), len(a.NonceKey))
	}
}

func TestWipeZeroes(t *testing.T) {
	k, err := NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	k.Wipe()
	for i, b := range k.DEK {
		if b != 0 {
			t.Fatalf("DEK[%d] = %d, want 0", i, b)
		}
	}
	for i, b := range k.NonceKey {
		if b != 0 {
			t.Fatalf("NonceKey[%d] = %d, want 0", i, b)
		}
	}
}

func TestKeyMaterialRedacted(t *testing.T) {
	k, err := NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer k.Wipe()
	all := fmt.Sprintf("%v %s %#v", k, k, k)
	if strings.Contains(all, string(k.DEK)) {
		t.Fatal("key material leaked through String")
	}
	if !strings.Contains(all, "REDACTED") {
		t.Fatal("String() must say REDACTED")
	}
}

func TestKeyMaterialValidate(t *testing.T) {
	k, err := NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer k.Wipe()
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := &KeyMaterial{SchemaVersion: 1, DEK: make([]byte, 16), NonceKey: make([]byte, 32)}
	if err := bad.Validate(); err == nil {
		t.Fatal("short DEK must fail validation")
	}
	bad2 := &KeyMaterial{SchemaVersion: 99, DEK: make([]byte, 32), NonceKey: make([]byte, 32)}
	if err := bad2.Validate(); err == nil {
		t.Fatal("bad schema must fail validation")
	}
	var nilKM *KeyMaterial
	if err := nilKM.Validate(); err == nil {
		t.Fatal("nil must fail validation")
	}
}

func TestKeyMaterialJSON(t *testing.T) {
	k, err := NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer k.Wipe()
	raw, err := k.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	back := &KeyMaterial{}
	if err := back.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	defer back.Wipe()
	if !bytes.Equal(back.DEK, k.DEK) || !bytes.Equal(back.NonceKey, k.NonceKey) {
		t.Fatal("JSON round-trip changed the secrets")
	}
}

func TestClone(t *testing.T) {
	k := newTestKM(t)
	c := k.Clone()
	if c == nil || k == c || bytes.Equal(k.DEK, make([]byte, 32)) {
		t.Fatalf("clone broken")
	}
	if !bytes.Equal(k.DEK, c.DEK) || !bytes.Equal(k.NonceKey, c.NonceKey) {
		t.Fatal("clone must copy secrets")
	}
	c.Wipe()
	if bytes.Equal(c.DEK, k.DEK) {
		t.Fatal("wiping clone must not wipe original")
	}
}

func TestWipeNil(t *testing.T) {
	var k *KeyMaterial
	k.Wipe()
}

func TestUnmarshalJSONErrors(t *testing.T) {
	k := &KeyMaterial{}
	if err := k.UnmarshalJSON([]byte("not json")); err == nil {
		t.Fatal("bad json must error")
	}
	if err := k.UnmarshalJSON([]byte(`{"schemaVersion":"bogus"}`)); err == nil {
		t.Fatal("wrong schema type must error")
	}
	if err := k.UnmarshalJSON([]byte(`{"schema_version":1,"dek":"!!!","nonce_key":"AQID"}`)); err == nil {
		t.Fatal("bad base64 must error")
	}
}

func TestValidateNilKMCoverage(t *testing.T) {
	var k *KeyMaterial
	if err := k.Validate(); err == nil {
		t.Fatal("nil validate must error")
	}
}

// TestAttestationSurvivesTheWrapping is the point of putting the epoch inside
// the material: what comes back out of the age blob is what went in, and it is
// authenticated by the wrapping rather than by a public field.
func TestAttestationSurvivesTheWrapping(t *testing.T) {
	km, err := NewDedupKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()
	var wrapped bytes.Buffer
	if err := WrapKeys(&wrapped, km, Recipients{Passphrase: []byte("pass")}); err != nil {
		t.Fatal(err)
	}
	back, err := UnwrapKeys(bytes.NewReader(wrapped.Bytes()), Identity{Passphrase: []byte("pass")})
	if err != nil {
		t.Fatal(err)
	}
	defer back.Wipe()
	if back.SchemaVersion != schemaVersion || back.EnvelopeVersion != EnvelopeVersion ||
		back.NonceMode != "convergent" || back.Reuse != ReuseConvergentDedup {
		t.Fatalf("attestation lost in the wrapping: %+v", struct {
			Schema, Envelope int
			Mode             string
			Reuse            ReusePolicy
		}{back.SchemaVersion, back.EnvelopeVersion, back.NonceMode, back.Reuse})
	}
	if err := back.ReusableFor(EnvelopeVersion, NonceConvergent); err != nil {
		t.Fatalf("a dedup key must come back reusable: %v", err)
	}
}

// TestLegacyMaterialStaysReadableAndUnusable covers both halves of the
// migration: a key file written before the attestation existed must keep
// opening its own backup, and must never seal a new one.
func TestLegacyMaterialStaysReadableAndUnusable(t *testing.T) {
	legacy := []byte(`{"schemaVersion":1,"dek":"` +
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)) + `","nonceKey":"` +
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)) + `"}`)
	var km KeyMaterial
	if err := json.Unmarshal(legacy, &km); err != nil {
		t.Fatal(err)
	}
	if err := km.Validate(); err != nil {
		t.Fatalf("legacy material must stay valid: %v", err)
	}
	if km.Attested() {
		t.Fatal("schema 1 material attests nothing")
	}
	if err := km.ReusableFor(EnvelopeVersion, NonceConvergent); !errors.Is(err, ErrKeyNotAttested) {
		t.Fatalf("ReusableFor = %v, want ErrKeyNotAttested", err)
	}
}

// TestReusableForRefusesEveryMismatch pins each refusal to its own error, so
// the caller can say which one happened instead of guessing.
func TestReusableForRefusesEveryMismatch(t *testing.T) {
	sound, err := NewDedupKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer sound.Wipe()

	older := sound.Clone()
	older.EnvelopeVersion = EnvelopeVersion - 1
	newer := sound.Clone()
	newer.EnvelopeVersion = EnvelopeVersion + 1
	randomMode := sound.Clone()
	randomMode.NonceMode = "random"
	single := sound.Clone()
	single.Reuse = ReuseNever

	cases := []struct {
		name string
		km   *KeyMaterial
		want error
	}{
		{"older envelope", older, ErrKeyEpoch},
		{"newer envelope", newer, ErrKeyEpoch},
		{"other nonce mode", randomMode, ErrKeyNonceMode},
		{"single use", single, ErrKeyNotReusable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.km.ReusableFor(EnvelopeVersion, NonceConvergent); !errors.Is(err, tc.want) {
				t.Fatalf("ReusableFor = %v, want %v", err, tc.want)
			}
		})
	}
	if err := sound.ReusableFor(EnvelopeVersion, NonceConvergent); err != nil {
		t.Fatalf("the sound key must stay reusable: %v", err)
	}
}

// TestValidateRejectsAnIncompleteAttestation keeps schema 2 honest: claiming
// the schema without carrying what it promises is not a valid key file.
func TestValidateRejectsAnIncompleteAttestation(t *testing.T) {
	base := func() *KeyMaterial {
		km, err := NewDedupKeyMaterial()
		if err != nil {
			t.Fatal(err)
		}
		return km
	}
	noEpoch := base()
	noEpoch.EnvelopeVersion = 0
	badMode := base()
	badMode.NonceMode = "sometimes"
	badReuse := base()
	badReuse.Reuse = "whenever"
	future := base()
	future.SchemaVersion = schemaVersion + 1

	for name, km := range map[string]*KeyMaterial{
		"no envelope version": noEpoch,
		"unknown nonce mode":  badMode,
		"unknown reuse":       badReuse,
		"unknown schema":      future,
	} {
		t.Run(name, func(t *testing.T) {
			defer km.Wipe()
			if err := km.Validate(); !errors.Is(err, ErrBadKeyMaterial) {
				t.Fatalf("Validate = %v, want ErrBadKeyMaterial", err)
			}
		})
	}
}
