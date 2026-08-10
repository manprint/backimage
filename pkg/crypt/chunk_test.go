package crypt

import (
	"bytes"
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
		blob, err := s.Seal(nil, 0, codec, payload, [32]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if len(blob) != 24+len(payload)+16 {
			t.Fatalf("blob size %d, want %d", len(blob), 24+len(payload)+16)
		}
		if h, _, err := ParseHeader(blob); err != nil || h.Flags != 0 {
			t.Fatalf("random-nonce mode flag check: h=%+v err=%v", h, err)
		}
		got, id, err := o.Open(nil, 0, blob)
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
	sha := sha256Sum([]byte("same"))
	a, err := s.Seal(nil, 0, codec, []byte("same"), sha)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Seal(nil, 0, codec, []byte("same"), sha)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("convergent seals must be identical for same payload")
	}
	shifted, err := s.Seal(nil, 7, codec, []byte("same"), sha)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, shifted) {
		t.Fatal("convergent seals must survive a changed chunk index")
	}
	c, err := s.Seal(nil, 0, codec, []byte("different"), sha256Sum([]byte("different")))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("different payloads must differ")
	}
	if h, _, err := ParseHeader(a); err != nil || h.Flags&flagConvergent == 0 {
		t.Fatalf("convergent seal must set flag: h=%+v err=%v", h, err)
	}
	got, _, err := o.Open(nil, 7, a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("same")) {
		t.Fatal("convergent open mismatch")
	}
	seen := map[[nonceLen]byte]bool{}
	for i := 0; i < 100; i++ {
		payload := []byte{byte(i), byte(i >> 8)}
		blob, err := s.Seal(nil, uint32(i+1), codec, payload, sha256Sum(payload))
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

func TestSealOpenChunkIndex(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceRandom)
	o := mustOpener(t, km)
	blob, err := s.Seal(nil, 7, testCodec(t), []byte("chunk"), [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := o.Open(nil, 7, blob); err != nil {
		t.Fatal("own index must open")
	}
	if _, _, err := o.Open(nil, 8, blob); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong index must be ErrIntegrity, got %v", err)
	}
}

func TestOpenBitFlips(t *testing.T) {
	km := newTestKM(t)
	blob, err := mustSealer(t, km, NonceRandom).Seal(nil, 0, testCodec(t), []byte("attack at dawn"), [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	o := mustOpener(t, km)
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 100; i++ {
		bad := append([]byte(nil), blob...)
		idx := r.Intn(len(bad))
		bad[idx] ^= 1 << uint(r.Intn(8))
		if _, _, err := o.Open(nil, 0, bad); err == nil {
			t.Fatalf("bitflip at %d passed authentication", idx)
		}
	}
}

func TestOpenAllocs(t *testing.T) {
	km := newTestKM(t)
	blob, err := mustSealer(t, km, NonceRandom).Seal(nil, 0, testCodec(t), bytes.Repeat([]byte{0}, 512), [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	o := mustOpener(t, km)
	allocs := testing.AllocsPerRun(100, func() {
		if _, _, err := o.Open(nil, 0, blob); err != nil {
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
	blob, err := s.Seal(nil, 0, testCodec(t), []byte("cleartext"), [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if h, n, err := ParseHeader(blob); err != nil || n != 12 || h.AEAD != aeadNone {
		t.Fatalf("clear envelope must be 12-byte header aead=0: h=%+v n=%d err=%v", h, n, err)
	}
	got, _, err := o.Open(nil, 0, blob)
	if err != nil || !bytes.Equal(got, []byte("cleartext")) {
		t.Fatal("clear envelope must open")
	}
}

func TestCipheredBlobNeedsKey(t *testing.T) {
	km := newTestKM(t)
	blob, err := mustSealer(t, km, NonceRandom).Seal(nil, 0, testCodec(t), []byte("secret"), [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mustOpener(t, nil).Open(nil, 0, blob); err == nil {
		t.Fatal("keyless opener must reject AES-GCM blobs")
	}
}

func TestNoncesNeverRepeat(t *testing.T) {
	km := newTestKM(t)
	s := mustSealer(t, km, NonceRandom)
	seen := make(map[[12]byte]bool)
	for i := 0; i < 100; i++ {
		blob, err := s.Seal(nil, uint32(i), testCodec(t), []byte("x"), [32]byte{})
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
