package crypt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The three tests below are the regression for the authentication bypass on
// the encrypted read path: one opener used to serve both kinds of blob, so a
// rewritten header saying aead=none made a reader holding the key return the
// payload without ever checking a tag.

func TestKeyedOpenerRejectsUnauthenticatedBlob(t *testing.T) {
	km := newTestKM(t)
	// What an attacker can produce without any key: a well-formed envelope
	// with no AEAD tag, carrying a payload of their choosing.
	forged, err := mustSealer(t, nil, NonceRandom).Seal(nil, RoleData, 0, testCodec(t), []byte("forged"))
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := ParseHeader(forged); err != nil || h.AEAD != aeadNone {
		t.Fatalf("the forged blob must parse as aead=none: %+v %v", h, err)
	}

	got, _, err := mustOpener(t, km).Open(nil, RoleData, 0, forged)
	if err == nil {
		t.Fatalf("a keyed opener must refuse an unauthenticated blob, it returned %q", got)
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("the refusal must be an integrity failure, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("no payload may be returned with the error, got %d bytes", len(got))
	}
}

func TestKeyedOpenerRejectsUnauthenticatedBlobForEveryRole(t *testing.T) {
	km := newTestKM(t)
	o := mustOpener(t, km)
	for _, role := range []Role{RoleData, RoleIndex, RolePrivate} {
		forged, err := mustSealer(t, nil, NonceRandom).Seal(nil, role, 0, testCodec(t), []byte("forged"))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := o.Open(nil, role, 0, forged); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("role %d: want ErrIntegrity, got %v", uint8(role), err)
		}
	}
}

func TestClearOpenerRejectsEncryptedBlob(t *testing.T) {
	km := newTestKM(t)
	sealed, err := mustSealer(t, km, NonceRandom).Seal(nil, RoleData, 0, testCodec(t), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = NewClearOpener().Open(nil, RoleData, 0, sealed)
	if err == nil {
		t.Fatal("a clear opener has no key and must say so instead of failing obscurely")
	}
	if !strings.Contains(err.Error(), "key material required") {
		t.Fatalf("the error must name the missing key, got %v", err)
	}
}

func TestClearOpenerStillReadsAClearBlob(t *testing.T) {
	blob, err := mustSealer(t, nil, NonceRandom).Seal(nil, RoleData, 0, testCodec(t), []byte("cleartext"))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := NewClearOpener().Open(nil, RoleData, 0, blob)
	if err != nil || !bytes.Equal(got, []byte("cleartext")) {
		t.Fatalf("an unencrypted backup must stay readable: %q %v", got, err)
	}
}

func TestNewKeyedOpenerRequiresKeyMaterial(t *testing.T) {
	if _, err := NewKeyedOpener(nil); err == nil {
		t.Fatal("a keyed opener without a key is the object this split exists to remove")
	}
}

func TestClearOpenerDeclaresItCannotAuthenticate(t *testing.T) {
	if NewClearOpener().RequiresAuthentication() {
		t.Fatal("the clear opener has no key and must not claim otherwise")
	}
	if !mustOpener(t, newTestKM(t)).RequiresAuthentication() {
		t.Fatal("the keyed opener must declare that it requires authentication")
	}
}
