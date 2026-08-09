package crypt

import (
	"bytes"
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
