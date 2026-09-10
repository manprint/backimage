package crypt

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/manprint/backimage/pkg/compress"
)

// TestTheNonceLabelFollowsTheEnvelopeVersion is the rule A19 asked to make
// structural. A convergent nonce derived under a label that stayed still
// while the envelope moved is a nonce two epochs of one key can share, and
// two GCM messages under one (key, nonce) hand over the GHASH authentication
// key of that DEK. The label is derived from the version, so this test states
// what the derivation guarantees rather than guarding a constant somebody has
// to remember to edit.
func TestTheNonceLabelFollowsTheEnvelopeVersion(t *testing.T) {
	want := "backimage/nonce/v" + strconv.Itoa(envelopeVersion) + "\x00"
	if nonceLabel != want {
		t.Fatalf("nonceLabel = %q, want %q: the label must move with the envelope version", nonceLabel, want)
	}
	if envelopeVersion == envelopeVersionLegacy {
		t.Fatal("the current envelope cannot be the legacy one")
	}
}

// TestTheConvergentNonceCoversEveryAuthenticatedField is the defect itself:
// the nonce used to be derived from role and payload while the AAD covered
// the whole header, so two blobs with the same payload and a different header
// got one nonce and two different authenticated messages.
func TestTheConvergentNonceCoversEveryAuthenticatedField(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	payload := []byte("the same bytes in every case")

	base := Header{Version: envelopeVersion, Codec: compress.Store, AEAD: aeadAES256GCM, Flags: flagConvergent}
	variant := func(mutate func(*Header), role Role) string {
		h := base
		mutate(&h)
		return string(convergentNonce(key, AAD(h, role, 0), payload))
	}
	reference := variant(func(*Header) {}, RoleData)

	cases := map[string]string{
		"another envelope version": variant(func(h *Header) { h.Version = envelopeVersionRole }, RoleData),
		"another codec":            variant(func(h *Header) { h.Codec = compress.Zstd }, RoleData),
		"another flag set":         variant(func(h *Header) { h.Flags |= 1 << 3 }, RoleData),
		"another role":             variant(func(*Header) {}, RolePrivate),
	}
	for name, got := range cases {
		if got == reference {
			t.Fatalf("%s produced the same nonce: an authenticated field is outside the derivation", name)
		}
	}

	// And the property deduplication depends on: at equal configuration the
	// same payload keeps producing the same nonce.
	if variant(func(*Header) {}, RoleData) != reference {
		t.Fatal("the same header and payload must produce the same nonce, or dedup stops working")
	}
}

// TestConvergentBlobsStillDeduplicate states the cost of the change at the
// level a user sees it: two sealings of one payload are byte for byte the
// same blob.
func TestConvergentBlobsStillDeduplicate(t *testing.T) {
	km, err := NewDedupKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()
	s, err := NewSealer(km, NonceConvergent)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("chunk"), 1000)
	first, err := s.Seal(nil, RoleData, 3, testCodec(t), payload)
	if err != nil {
		t.Fatal(err)
	}
	// A different chunk index on purpose: a convergent blob does not bind its
	// position, which is what lets a moved chunk still deduplicate.
	second, err := s.Seal(nil, RoleData, 41, testCodec(t), payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two sealings of one payload must be the same blob")
	}
}
