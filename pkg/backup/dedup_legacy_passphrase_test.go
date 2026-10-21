package backup

import (
	"bytes"
	"testing"

	"github.com/manprint/backimage/pkg/crypt"
)

// A base sealed by a release that read the passphrase input differently (the
// whole file, "pass\n\n" giving "pass\n") is still the base to deduplicate
// against: its key reopens with the earlier reading and is reused, instead of
// forcing a full re-upload behind a "passphrase diversa" warning.
func TestDedupReusesABaseSealedUnderAnEarlierReading(t *testing.T) {
	km, err := crypt.NewDedupKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()
	var wrapped bytes.Buffer
	if err := crypt.WrapKeys(&wrapped, km, crypt.Recipients{Passphrase: []byte("pass\n")}); err != nil {
		t.Fatal(err)
	}
	base := baseWith(t, km, currentEncryption(), "base")
	base.keyFiles["keys.pass.age"] = wrapped.Bytes()

	fallback := []byte("pass\n")
	cfg := Config{Encrypt: true, Dedup: true, PassphraseFallbacks: func() [][]byte { return [][]byte{fallback} }}
	got, refusal := reuseDedupKey(base, cfg, []byte("pass"))
	if got == nil {
		t.Fatalf("base key not reused: %q", refusal)
	}
	defer got.Wipe()
	if !bytes.Equal(got.DEK, km.DEK) {
		t.Fatal("reused key differs from the base key")
	}
	if !bytes.Equal(fallback, make([]byte, len(fallback))) {
		t.Fatal("the earlier reading was not wiped after use")
	}

	// Without the earlier reading the same base is a different passphrase.
	if got, _ := reuseDedupKey(base, Config{Encrypt: true, Dedup: true}, []byte("pass")); got != nil {
		got.Wipe()
		t.Fatal("base reopened without the earlier reading")
	}
}
