package crypt

import (
	"encoding/hex"
	"testing"
)

// TestGoldenConvergentVector locks the wire format of the convergent-mode
// envelope to catch accidental format drift across refactors.
//
// Deterministic inputs: fixed KeyMaterial, codec=store, chunk index 0,
// payload "vector", plaintext digest of "vector".
// Breaking this test intentionally = bump envelopeVersion and re-document.
func TestGoldenConvergentVector(t *testing.T) {
	dek, _ := hex.DecodeString("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	nonce, _ := hex.DecodeString("ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100")
	km := &KeyMaterial{SchemaVersion: 1, DEK: dek, NonceKey: nonce}
	defer km.Wipe()
	s, err := NewSealer(km, NonceConvergent)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Seal(nil, 0, testCodec(t), []byte("vector"), sha256Sum([]byte("vector")))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("42494d4743484b31010001015268563fc9544ae32ea7cbc61f8801e0437df8b46f6321a679d33d5cfb041a40fa54")
	if got := hex.EncodeToString(blob); got != hex.EncodeToString(want) {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", hex.EncodeToString(blob), hex.EncodeToString(want))
	}
	o, err := NewOpener(km)
	if err != nil {
		t.Fatal(err)
	}
	pt, _, err := o.Open(nil, 0, blob)
	if err != nil || string(pt) != "vector" {
		t.Fatalf("golden vector must open: %q %v", pt, err)
	}
}
