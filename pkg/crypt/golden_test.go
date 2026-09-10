package crypt

import (
	"encoding/hex"
	"testing"
)

// TestGoldenConvergentVector locks the wire format of the convergent-mode
// envelope to catch accidental format drift across refactors.
//
// Deterministic inputs: fixed KeyMaterial, codec=store, role=data, chunk
// index 0, payload "vector". The vector has changed twice, each time together
// with the envelope version: in 0.2.4 (v2) the convergent nonce moved to the
// sealed payload and the AAD gained the role; in 0.5.0 (v3) the nonce is
// derived from the AAD as well, so a version bump moves it too. Breaking this
// test intentionally = bump envelopeVersion and re-document.
func TestGoldenConvergentVector(t *testing.T) {
	km := goldenKeyMaterial()
	defer km.Wipe()
	s, err := NewSealer(km, NonceConvergent)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Seal(nil, RoleData, 0, testCodec(t), []byte("vector"))
	if err != nil {
		t.Fatal(err)
	}
	want := "42494d4743484b3103000101a4e94ec22c8a2a2a83cb0d5d506c800dd063410c896a8685269b00a21e57c0fec1f7"
	if got := hex.EncodeToString(blob); got != want {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", got, want)
	}
	o, err := NewKeyedOpener(km)
	if err != nil {
		t.Fatal(err)
	}
	pt, _, err := o.Open(nil, RoleData, 0, blob)
	if err != nil || string(pt) != "vector" {
		t.Fatalf("golden vector must open: %q %v", pt, err)
	}
}

// TestGoldenVectorsOfEveryPublishedEnvelopeStillOpen is the read side of the
// same table. Each of these blobs was written by a release that is out there;
// the day one of them stops opening, a published backup has become
// unreadable.
func TestGoldenVectorsOfEveryPublishedEnvelopeStillOpen(t *testing.T) {
	km := goldenKeyMaterial()
	defer km.Wipe()
	o, err := NewKeyedOpener(km)
	if err != nil {
		t.Fatal(err)
	}
	published := map[string]string{
		// 0.2.4 through 0.4.0: nonce from the payload, role in the AAD.
		"envelope v2": "42494d4743484b3102000101f1d50e175143ea9cd2111471cf9e115b36cb947c49afae148e172e6b969baa973d26",
	}
	for name, encoded := range published {
		t.Run(name, func(t *testing.T) {
			blob, err := hex.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			pt, _, err := o.Open(nil, RoleData, 0, blob)
			if err != nil || string(pt) != "vector" {
				t.Fatalf("a published envelope stopped opening: %q %v", pt, err)
			}
		})
	}
}

func goldenKeyMaterial() *KeyMaterial {
	dek, _ := hex.DecodeString("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	nonce, _ := hex.DecodeString("ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100")
	return &KeyMaterial{SchemaVersion: 1, DEK: dek, NonceKey: nonce}
}
