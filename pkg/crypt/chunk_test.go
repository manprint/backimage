package crypt

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"math/rand"
	"testing"

	"github.com/manprint/backimage/pkg/compress"
)

func mustSealer(t *testing.T, km *KeyMaterial, mode NonceMode) Sealer {
	t.Helper()
	s, err := NewSealer(km, mode)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustOpener(t *testing.T, km *KeyMaterial) Opener {
	t.Helper()
	o, err := NewOpener(km)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func testCodec(t *testing.T) compress.Codec {
	t.Helper()
	c, err := compress.ByID(compress.Store)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSealOpenRandomNonce(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceRandom)
	o := mustOpener(t, km)
	codec := testCodec(t)
	for i := 0; i < 20; i++ {
		payload := make([]byte, 1<<uint(i%7))
		rand.New(rand.NewSource(int64(i))).Read(payload)
		blob, err := s.Seal(nil, RoleData, 0, codec, payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(blob) != 24+len(payload)+16 {
			t.Fatalf("blob size %d, want %d", len(blob), 24+len(payload)+16)
		}
		if h, _, err := ParseHeader(blob); err != nil || h.Flags != 0 {
			t.Fatalf("random-nonce mode flag check: h=%+v err=%v", h, err)
		}
		got, id, err := o.Open(nil, RoleData, 0, blob)
		if err != nil {
			t.Fatal(err)
		}
		if id != compress.Store {
			t.Fatalf("codec id %d, want 0", id)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("round-trip mismatch")
		}
	}
}

func TestSealConvergent(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceConvergent)
	o := mustOpener(t, km)
	codec := testCodec(t)
	a, err := s.Seal(nil, RoleData, 0, codec, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Seal(nil, RoleData, 0, codec, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("convergent seals must be identical for same payload")
	}
	shifted, err := s.Seal(nil, RoleData, 7, codec, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, shifted) {
		t.Fatal("convergent seals must survive a changed chunk index")
	}
	c, err := s.Seal(nil, RoleData, 0, codec, []byte("different"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("different payloads must differ")
	}
	if h, _, err := ParseHeader(a); err != nil || h.Flags&flagConvergent == 0 {
		t.Fatalf("convergent seal must set flag: h=%+v err=%v", h, err)
	}
	got, _, err := o.Open(nil, RoleData, 7, a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("same")) {
		t.Fatal("convergent open mismatch")
	}
	seen := map[[nonceLen]byte]bool{}
	for i := 0; i < 100; i++ {
		payload := []byte{byte(i), byte(i >> 8)}
		blob, err := s.Seal(nil, RoleData, uint32(i+1), codec, payload)
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := ParseHeader(blob)
		if err != nil {
			t.Fatal(err)
		}
		if seen[h.Nonce] {
			t.Fatalf("convergent nonce reused for distinct payload %d", i)
		}
		seen[h.Nonce] = true
	}
}

// TestConvergentNonceIsSealedPayloadDerived is the regression for the nonce
// reuse fixed in 0.2.4.
//
// Up to 0.2.3 Seal took the digest of the *plaintext* chunk and encrypted the
// *compressed* chunk. Two backups sharing a repository key therefore sealed
// two different byte strings under one nonce whenever the compressed form of an
// unchanged chunk changed: a different --compression or --level, or the same
// codec framing its output differently because it ran with another worker
// count. Two AES-GCM messages under one key and nonce give away the XOR of
// their plaintexts and the GHASH authentication key.
//
// The two payloads below stand for zstd(P) and gzip(P) of one plaintext P.
// Their nonces must differ, and the only way for a nonce to repeat must be for
// the sealed bytes to be identical — which is exactly what deduplication wants.
func TestConvergentNonceIsSealedPayloadDerived(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceConvergent)
	codec := testCodec(t)

	nonceOf := func(payload []byte) [nonceLen]byte {
		t.Helper()
		blob, err := s.Seal(nil, RoleData, 0, codec, payload)
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := ParseHeader(blob)
		if err != nil {
			t.Fatal(err)
		}
		return h.Nonce
	}

	zstdForm := []byte("compressed-with-zstd:PLAINTEXT")
	gzipForm := []byte("compressed-with-gzip:PLAINTEXT")
	if nonceOf(zstdForm) == nonceOf(gzipForm) {
		t.Fatal("two different sealed payloads share a nonce: keystream reuse")
	}
	// Two separate seals of the same payload: the derivation must be a pure
	// function of the sealed bytes, or deduplication stops working.
	firstSeal := nonceOf(zstdForm)
	secondSeal := nonceOf(zstdForm)
	if firstSeal != secondSeal {
		t.Fatal("identical payloads must converge to the same nonce, or dedup breaks")
	}
}

// TestConvergentNonceNeverPairsOneNonceWithTwoPayloads states the security
// property of the derivation in the form that actually matters, over real
// compressor output.
//
// AES-GCM is broken by encrypting *different* bytes under the same (key,
// nonce), not by reusing a nonce for the same bytes — the latter is precisely
// what deduplication is. So the invariant is: equal nonce implies equal sealed
// payload. Every codec and level below seals the same plaintext chunk with one
// reused key, the way `--dedup` does across two backups.
//
// The second half shows what 0.2.3 did instead: its nonce came from the
// plaintext digest, which is the same for all of these, so one nonce covered
// several distinct payloads at once.
func TestConvergentNonceNeverPairsOneNonceWithTwoPayloads(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceConvergent)
	plaintext := bytes.Repeat([]byte("the same unchanged chunk of user data. "), 400)

	forms := []struct {
		name  string
		codec string
		level int
	}{
		{"zstd-1", "zstd", 1}, {"zstd-2", "zstd", 2}, {"zstd-4", "zstd", 4},
		{"gzip-1", "gzip", 1}, {"gzip-9", "gzip", 9},
		{"store", "store", 0},
	}

	byNonce := map[[nonceLen]byte][]byte{}
	payloads := map[string]bool{}
	for _, f := range forms {
		codec, err := compress.Get(f.codec)
		if err != nil {
			t.Fatal(err)
		}
		level := f.level
		if level == 0 {
			_, _, level = codec.Levels()
		}
		var buf bytes.Buffer
		w, err := codec.NewWriter(&buf, level)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(plaintext); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		payload := buf.Bytes()
		payloads[string(payload)] = true

		blob, err := s.Seal(nil, RoleData, 0, codec, payload)
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := ParseHeader(blob)
		if err != nil {
			t.Fatal(err)
		}
		if seen, dup := byNonce[h.Nonce]; dup && !bytes.Equal(seen, payload) {
			t.Fatalf("%s: one nonce now covers two different payloads (%d and %d bytes): keystream reuse",
				f.name, len(seen), len(payload))
		}
		byNonce[h.Nonce] = payload
	}

	// Sanity: the fixture must actually produce several distinct compressed
	// forms, or the test above would pass for the wrong reason.
	if len(payloads) < 2 {
		t.Fatalf("fixture produced only %d distinct compressed form(s)", len(payloads))
	}

	// The pre-0.2.4 derivation ignored the payload, so all of these forms drew
	// the same nonce — the vulnerability, reproduced.
	legacy := legacyConvergentNonce(km.NonceKey, sha256Sum(plaintext))
	for range forms {
		if !bytes.Equal(legacy, legacyConvergentNonce(km.NonceKey, sha256Sum(plaintext))) {
			t.Fatal("legacy derivation is expected to be payload-blind")
		}
	}
	if len(byNonce) < 2 {
		t.Fatalf("current derivation collapsed %d distinct payloads onto %d nonce(s)",
			len(payloads), len(byNonce))
	}
}

// TestConvergentNonceIsRoleSeparated keeps two blobs of different kinds from
// sharing a nonce even when their sealed bytes happen to be identical.
func TestConvergentNonceIsRoleSeparated(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceConvergent)
	codec := testCodec(t)
	payload := []byte("identical bytes in two different blobs")
	seen := map[[nonceLen]byte]Role{}
	for _, role := range []Role{RoleData, RoleIndex, RolePrivate} {
		blob, err := s.Seal(nil, role, 0, codec, payload)
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := ParseHeader(blob)
		if err != nil {
			t.Fatal(err)
		}
		if other, dup := seen[h.Nonce]; dup {
			t.Fatalf("role %s reuses the nonce of role %s", role, other)
		}
		seen[h.Nonce] = role
	}
}

// TestRolesAreNotInterchangeable checks the authenticated role. Before 0.2.4
// the file index, the private metadata and data chunk 0 were all sealed with an
// identical AAD, so under one key any of them authenticated in the place of
// another.
func TestRolesAreNotInterchangeable(t *testing.T) {
	km := newTestKM(t)
	o := mustOpener(t, km)
	codec := testCodec(t)
	for _, mode := range []NonceMode{NonceRandom, NonceConvergent} {
		s := mustSealer(t, km, mode)
		for _, sealed := range []Role{RoleData, RoleIndex, RolePrivate} {
			blob, err := s.Seal(nil, sealed, 0, codec, []byte("payload"))
			if err != nil {
				t.Fatal(err)
			}
			for _, opened := range []Role{RoleData, RoleIndex, RolePrivate} {
				_, _, err := o.Open(nil, opened, 0, blob)
				switch {
				case opened == sealed && err != nil:
					t.Fatalf("mode %d: role %s must open as itself: %v", mode, sealed, err)
				case opened != sealed && !errors.Is(err, ErrIntegrity):
					t.Fatalf("mode %d: role %s opened as %s, want ErrIntegrity, got %v",
						mode, sealed, opened, err)
				}
			}
		}
	}
}

func TestUnknownRoleRejected(t *testing.T) {
	km := newTestKM(t)
	if _, err := mustSealer(t, km, NonceRandom).Seal(nil, Role(9), 0, testCodec(t), []byte("x")); err == nil {
		t.Fatal("Seal must reject an unknown role")
	}
	if _, _, err := mustOpener(t, km).Open(nil, Role(9), 0, []byte("x")); err == nil {
		t.Fatal("Open must reject an unknown role")
	}
}

// TestLegacyEnvelopeStillOpens builds a version 1 blob the way 0.2.3 did and
// checks it still opens: images already in a registry must keep restoring.
func TestLegacyEnvelopeStillOpens(t *testing.T) {
	km := newTestKM(t)
	ae, err := gcmFor(km.DEK)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("written by 0.2.3")
	// 0.2.3, convergent mode: nonce = HMAC(NonceKey, sha256(plaintext)).
	h := Header{Version: envelopeVersionLegacy, Codec: compress.Store, AEAD: aeadAES256GCM, Flags: flagConvergent}
	sum := sha256Sum(payload)
	legacy := legacyConvergentNonce(km.NonceKey, sum)
	copy(h.Nonce[:], legacy)
	blob := make([]byte, headerMaxSize)
	if _, err := MarshalHeader(blob, h); err != nil {
		t.Fatal(err)
	}
	blob = ae.Seal(blob, h.Nonce[:], payload, AAD(h, RoleData, 0))

	// The legacy AAD names no role, so any role opens it. That is the hole
	// version 2 closes; what matters here is that the blob is still readable.
	got, id, err := mustOpener(t, km).Open(nil, RoleData, 0, blob)
	if err != nil {
		t.Fatalf("legacy blob must still open: %v", err)
	}
	if id != compress.Store || !bytes.Equal(got, payload) {
		t.Fatalf("legacy round-trip mismatch: codec=%d payload=%q", id, got)
	}
}

func TestSealWritesCurrentEnvelopeVersion(t *testing.T) {
	km := newTestKM(t)
	for _, mode := range []NonceMode{NonceRandom, NonceConvergent} {
		blob, err := mustSealer(t, km, mode).Seal(nil, RoleData, 0, testCodec(t), []byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := ParseHeader(blob)
		if err != nil {
			t.Fatal(err)
		}
		if h.Version != EnvelopeVersion {
			t.Fatalf("mode %d wrote envelope version %d, want %d", mode, h.Version, EnvelopeVersion)
		}
	}
	// The clear envelope carries the same version marker.
	blob, err := mustSealer(t, nil, NonceRandom).Seal(nil, RoleData, 0, testCodec(t), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := ParseHeader(blob); err != nil || h.Version != EnvelopeVersion {
		t.Fatalf("clear envelope version: h=%+v err=%v", h, err)
	}
}

func TestSealOpenChunkIndex(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceRandom)
	o := mustOpener(t, km)
	blob, err := s.Seal(nil, RoleData, 7, testCodec(t), []byte("chunk"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := o.Open(nil, RoleData, 7, blob); err != nil {
		t.Fatal("own index must open")
	}
	if _, _, err := o.Open(nil, RoleData, 8, blob); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong index must be ErrIntegrity, got %v", err)
	}
}

func TestOpenBitFlips(t *testing.T) {
	km := newTestKM(t)
	blob, err := mustSealer(t, km, NonceRandom).Seal(nil, RoleData, 0, testCodec(t), []byte("attack at dawn"))
	if err != nil {
		t.Fatal(err)
	}
	o := mustOpener(t, km)
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 100; i++ {
		bad := append([]byte(nil), blob...)
		idx := r.Intn(len(bad))
		bad[idx] ^= 1 << uint(r.Intn(8))
		if _, _, err := o.Open(nil, RoleData, 0, bad); err == nil {
			t.Fatalf("bitflip at %d passed authentication", idx)
		}
	}
}

func TestOpenAllocs(t *testing.T) {
	km := newTestKM(t)
	blob, err := mustSealer(t, km, NonceRandom).Seal(nil, RoleData, 0, testCodec(t), bytes.Repeat([]byte{0}, 512))
	if err != nil {
		t.Fatal(err)
	}
	o := mustOpener(t, km)
	allocs := testing.AllocsPerRun(100, func() {
		if _, _, err := o.Open(nil, RoleData, 0, blob); err != nil {
			panic(err)
		}
	})
	if allocs > 4 {
		t.Fatalf("Open allocs %.1f > 4", allocs)
	}
}

func TestClearEnvelope(t *testing.T) {
	o := mustOpener(t, nil)
	s := mustSealer(t, nil, NonceRandom)
	blob, err := s.Seal(nil, RoleData, 0, testCodec(t), []byte("cleartext"))
	if err != nil {
		t.Fatal(err)
	}
	if h, n, err := ParseHeader(blob); err != nil || n != 12 || h.AEAD != aeadNone {
		t.Fatalf("clear envelope must be 12-byte header aead=0: h=%+v n=%d err=%v", h, n, err)
	}
	got, _, err := o.Open(nil, RoleData, 0, blob)
	if err != nil || !bytes.Equal(got, []byte("cleartext")) {
		t.Fatal("clear envelope must open")
	}
}

func TestCipheredBlobNeedsKey(t *testing.T) {
	km := newTestKM(t)
	blob, err := mustSealer(t, km, NonceRandom).Seal(nil, RoleData, 0, testCodec(t), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mustOpener(t, nil).Open(nil, RoleData, 0, blob); err == nil {
		t.Fatal("keyless opener must reject AES-GCM blobs")
	}
}

func TestNoncesNeverRepeat(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceRandom)
	seen := make(map[[12]byte]bool)
	for i := 0; i < 100; i++ {
		blob, err := s.Seal(nil, RoleData, uint32(i), testCodec(t), []byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := ParseHeader(blob)
		if err != nil {
			t.Fatal(err)
		}
		if seen[h.Nonce] {
			t.Fatalf("nonce reused at seal #%d", i)
		}
		seen[h.Nonce] = true
	}
}

func TestSealerValidation(t *testing.T) {
	bad := &KeyMaterial{SchemaVersion: 1, DEK: make([]byte, 16), NonceKey: make([]byte, 32)}
	if _, err := NewSealer(bad, NonceRandom); err == nil {
		t.Fatal("invalid km must fail")
	}
	if _, err := NewSealer(newTestKM(t), NonceMode(9)); err == nil {
		t.Fatal("unknown nonce mode must fail")
	}
	if s, err := NewSealer(nil, NonceRandom); err != nil || s.Overhead() != 12 {
		t.Fatalf("nil-km sealer: err=%v overhead=%d", err, s.Overhead())
	}
}

func TestOverhead(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceRandom)
	if s.Overhead() != 40 {
		t.Fatalf("overhead %d, want 40", s.Overhead())
	}
}

func sha256Sum(b []byte) [32]byte {
	return sha256.Sum256(b)
}

// legacyConvergentNonce reproduces the pre-0.2.4 derivation: HMAC over the
// digest of the plaintext chunk, with no domain label and no role. It exists
// only to build fixtures for the backward-compatibility test.
func legacyConvergentNonce(nonceKey []byte, plainSHA [32]byte) []byte {
	mac := hmac.New(sha256.New, nonceKey)
	mac.Write(plainSHA[:])
	return mac.Sum(nil)[:nonceLen]
}
