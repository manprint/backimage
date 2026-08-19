package crypt

import (
	"encoding/hex"
	"testing"
)

// TestGoldenConvergentVector locks the wire format of the convergent-mode
// envelope to catch accidental format drift across refactors.
//
// Deterministic inputs: fixed KeyMaterial, codec=store, role=data, chunk
// index 0, payload "vector". The vector changed once, in 0.2.4, together with
// the envelope version: the convergent nonce is now derived from the sealed
// payload and the AAD names the role. Breaking this test intentionally = bump
// envelopeVersion and re-document.
func TestGoldenConvergentVector(t *testing.T) {
	dek, _ := hex.DecodeString("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	nonce, _ := hex.DecodeString("ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100")
	km := &KeyMaterial{SchemaVersion: 1, DEK: dek, NonceKey: nonce}
	defer km.Wipe()
	s, err := NewSealer(km, NonceConvergent)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Seal(nil, RoleData, 0, testCodec(t), []byte("vector"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("42494d4743484b3102000101f1d50e175143ea9cd2111471cf9e115b36cb947c49afae148e172e6b969baa973d26")
	if got := hex.EncodeToString(blob); got != hex.EncodeToString(want) {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", hex.EncodeToString(blob), hex.EncodeToString(want))
	}
	o, err := NewOpener(km)
	if err != nil {
		t.Fatal(err)
	}
	pt, _, err := o.Open(nil, RoleData, 0, blob)
	if err != nil || string(pt) != "vector" {
		t.Fatalf("golden vector must open: %q %v", pt, err)
	}
}
