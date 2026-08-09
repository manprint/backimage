package crypt

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func newTestKM(t *testing.T) *KeyMaterial {
	t.Helper()
	km, err := NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(km.Wipe)
	return km
}

func TestWrapUnwrapPassphrase(t *testing.T) {
	km := newTestKM(t)
	var buf bytes.Buffer
	if err := WrapKeys(&buf, km, Recipients{Passphrase: []byte("testpass")}); err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKeys(&buf, Identity{Passphrase: []byte("testpass")})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !bytes.Equal(got.DEK, km.DEK) || !bytes.Equal(got.NonceKey, km.NonceKey) {
		t.Fatal("unwrapped key material differs")
	}
}

func TestWrapUnwrapWrongPassphrase(t *testing.T) {
	km := newTestKM(t)
	var buf bytes.Buffer
	if err := WrapKeys(&buf, km, Recipients{Passphrase: []byte("right")}); err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapKeys(&buf, Identity{Passphrase: []byte("wrong")}); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("want ErrWrongPassphrase, got %v", err)
	}
}

func TestWrapUnwrapAgeKey(t *testing.T) {
	km := newTestKM(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WrapKeys(&buf, km, Recipients{AgeKeys: []string{id.Recipient().String()}}); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(keyFile, []byte(id.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKeys(&buf, Identity{AgeKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.DEK, km.DEK) {
		t.Fatal("key material mismatch after age unwrap")
	}
}

func TestWrapMixedRecipientsErrors(t *testing.T) {
	km := newTestKM(t)
	id, _ := age.GenerateX25519Identity()
	var buf bytes.Buffer
	err := WrapKeys(&buf, km, Recipients{Passphrase: []byte("p"), AgeKeys: []string{id.Recipient().String()}})
	if !errors.Is(err, ErrMixedRecipients) {
		t.Fatalf("mixed recipients must error, got %v", err)
	}
	// ...and both files must be producible and openable separately.
	var pass, key bytes.Buffer
	if err := WrapKeys(&pass, km, Recipients{Passphrase: []byte("p")}); err != nil {
		t.Fatal(err)
	}
	if err := WrapKeys(&key, km, Recipients{AgeKeys: []string{id.Recipient().String()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapKeys(&pass, Identity{Passphrase: []byte("p")}); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "k.txt")
	os.WriteFile(keyFile, []byte(id.String()), 0o600)
	if _, err := UnwrapKeys(&key, Identity{AgeKeyFile: keyFile}); err != nil {
		t.Fatal(err)
	}
}

func TestWrapNoRecipients(t *testing.T) {
	km := newTestKM(t)
	err := WrapKeys(io.Discard, km, Recipients{})
	if err == nil || !strings.Contains(err.Error(), "no recipients") {
		t.Fatalf("empty recipients must error, got %v", err)
	}
}

func TestUnwrapTruncatedFile(t *testing.T) {
	km := newTestKM(t)
	var buf bytes.Buffer
	if err := WrapKeys(&buf, km, Recipients{Passphrase: []byte("p")}); err != nil {
		t.Fatal(err)
	}
	half := buf.Bytes()[:buf.Len()/2]
	if _, err := UnwrapKeys(bytes.NewReader(half), Identity{Passphrase: []byte("p")}); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("truncated key file must fail, got %v", err)
	}
}

func TestGoldenKeysAge(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "keys.age"))
	if err != nil {
		t.Skipf("golden missing: %v", err)
	}
	defer f.Close()
	km, err := UnwrapKeys(f, Identity{Passphrase: []byte("testpass")})
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()
	wantDEK := mustHex("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if !bytes.Equal(km.DEK, wantDEK) {
		t.Fatalf("golden DEK mismatch: %x", km.DEK)
	}
}

func mustHex(s string) []byte {
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, lo := hexval(s[2*i]), hexval(s[2*i+1])
		out[i] = hi<<4 | lo
	}
	return out
}

func hexval(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	panic("bad hex")
}

func TestWrapKeysErrors(t *testing.T) {
	km := newTestKM(t)
	id, _ := age.GenerateX25519Identity()
	if err := WrapKeys(io.Discard, km, Recipients{AgeKeys: []string{id.Recipient().String(), id.Recipient().String()}}); err == nil {
		t.Fatal("multiple age keys must error")
	}
	if err := WrapKeys(io.Discard, km, Recipients{AgeKeys: []string{"age1garbage"}}); err == nil {
		t.Fatal("bad age recipient must error")
	}
	if err := WrapKeys(io.Discard, km, Recipients{Passphrase: []byte("")}); err == nil || !strings.Contains(err.Error(), "no recipients") {
		t.Fatalf("empty passphrase list must be treated as no recipients: %v", err)
	}
}

func TestWrapKeysWriteError(t *testing.T) {
	km := newTestKM(t)
	err := WrapKeys(&failWriter{}, km, Recipients{Passphrase: []byte("p")})
	if err == nil {
		t.Fatal("write failure must surface")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestUnwrapKeysErrors(t *testing.T) {
	var buf bytes.Buffer
	km := newTestKM(t)
	if err := WrapKeys(&buf, km, Recipients{Passphrase: []byte("p")}); err != nil {
		t.Fatal(err)
	}
	// no identity at all
	if _, err := UnwrapKeys(bytes.NewReader(buf.Bytes()), Identity{}); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("no identities: %v", err)
	}
	// empty stream
	if _, err := UnwrapKeys(bytes.NewReader(nil), Identity{Passphrase: []byte("p")}); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("empty file: %v", err)
	}
	// corrupt the armored body: age ignores the human-readable labels, so
	// flip a body character instead of the header line
	lines := bytes.Split(buf.Bytes(), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if len(lines[i]) > 40 && !bytes.HasPrefix(lines[i], []byte("-----")) {
			lines[i][0] ^= 1
			break
		}
	}
	corrupt := bytes.Join(lines, []byte("\n"))
	if _, err := UnwrapKeys(bytes.NewReader(corrupt), Identity{Passphrase: []byte("p")}); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("corrupt body: %v", err)
	}
	// identity file that does not exist
	if _, err := UnwrapKeys(bytes.NewReader(buf.Bytes()), Identity{AgeKeyFile: "/nonexistent/identity.txt"}); err == nil {
		t.Fatal("missing identity file must error")
	}
}

func TestUnwrapKeysBadIdentityFile(t *testing.T) {
	km := newTestKM(t)
	var buf bytes.Buffer
	if err := WrapKeys(&buf, km, Recipients{Passphrase: []byte("p")}); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "id.txt")
	os.WriteFile(f, []byte("not a key"), 0o600)
	if _, err := UnwrapKeys(bytes.NewReader(buf.Bytes()), Identity{AgeKeyFile: f}); err == nil {
		t.Fatal("unparseable identity file must error")
	}
}

func TestWrapWrongPassFile(t *testing.T) {
	km := newTestKM(t)
	id, _ := age.GenerateX25519Identity()
	keyFile := filepath.Join(t.TempDir(), "id.txt")
	os.WriteFile(keyFile, []byte(id.String()), 0o600)
	var buf bytes.Buffer
	if err := WrapKeys(&buf, km, Recipients{AgeKeys: []string{id.Recipient().String()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapKeys(bytes.NewReader(buf.Bytes()), Identity{Passphrase: []byte("wrong")}); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("passphrase vs age blob must fail cleanly: %v", err)
	}
}
