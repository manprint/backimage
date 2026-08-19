package crypt

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// brutalPassphrase carries every printable ASCII punctuation mark, both letter
// cases, digits, an embedded space, an embedded carriage return, and multi-byte
// UTF-8. Nothing in the passphrase path may normalise, split, trim or re-encode
// any of it: the bytes that reach age's scrypt must be the bytes the user gave.
const brutalPassphrase = "aZ0 !\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~\rèñ日本語🔐 trailing "

func TestReadPassphrasePreservesEveryByte(t *testing.T) {
	// Direct: what --password hands over.
	t.Run("direct", func(t *testing.T) {
		got, err := ReadPassphrase(PassphraseSource{Direct: []byte(brutalPassphrase)})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != brutalPassphrase {
			t.Fatalf("direct mangled the passphrase:\n got %q\nwant %q", got, brutalPassphrase)
		}
	})

	// File: --passphrase-file. Exactly one trailing newline is the separator
	// and is removed; everything before it, spaces included, is the passphrase.
	t.Run("file", func(t *testing.T) {
		for name, written := range map[string]string{
			"lf":   brutalPassphrase + "\n",
			"crlf": brutalPassphrase + "\r\n",
			"bare": brutalPassphrase,
		} {
			path := filepath.Join(t.TempDir(), "pass-"+name)
			if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadPassphrase(PassphraseSource{File: path})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if string(got) != brutalPassphrase {
				t.Fatalf("%s mangled the passphrase:\n got %q\nwant %q", name, got, brutalPassphrase)
			}
		}
	})

	// Env: BACKIMAGE_PASSPHRASE, the path `docker run -e` uses.
	t.Run("env", func(t *testing.T) {
		t.Setenv("BACKIMAGE_PASSPHRASE", brutalPassphrase)
		got, err := ReadPassphrase(PassphraseSource{})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != brutalPassphrase {
			t.Fatalf("env mangled the passphrase:\n got %q\nwant %q", got, brutalPassphrase)
		}
	})
}

// TestReadPassphraseStdinPreservesEveryByte covers --passphrase-stdin, which
// reads one line: everything up to the first newline, that byte excluded.
func TestReadPassphraseStdinPreservesEveryByte(t *testing.T) {
	// The \r inside brutalPassphrase is fine on stdin; a \n would end the line
	// by definition, so it cannot be part of a passphrase read this way.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	go func() {
		w.Write([]byte(brutalPassphrase + "\ntrailing junk\n"))
		w.Close()
	}()
	got, err := ReadPassphrase(PassphraseSource{Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != brutalPassphrase {
		t.Fatalf("stdin mangled the passphrase:\n got %q\nwant %q", got, brutalPassphrase)
	}
}

// TestWrapUnwrapWithSpecialCharacters proves the whole key-file path is
// byte-exact: a passphrase full of punctuation and multi-byte runes wraps and
// unwraps, and any single-byte difference is rejected.
func TestWrapUnwrapWithSpecialCharacters(t *testing.T) {
	km := newTestKM(t)
	var buf bytes.Buffer
	if err := WrapKeys(&buf, km, Recipients{Passphrase: []byte(brutalPassphrase)}); err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKeys(bytes.NewReader(buf.Bytes()), Identity{Passphrase: []byte(brutalPassphrase)})
	if err != nil {
		t.Fatalf("special-character passphrase must unwrap: %v", err)
	}
	defer got.Wipe()
	if !bytes.Equal(got.DEK, km.DEK) || !bytes.Equal(got.NonceKey, km.NonceKey) {
		t.Fatal("unwrapped key material differs")
	}

	// Every near-miss must fail, including the ones a careless trim would
	// produce: a lost trailing space, a lost \r, a re-encoded rune.
	for name, wrong := range map[string]string{
		"trimmed trailing space": brutalPassphrase[:len(brutalPassphrase)-1],
		"stripped cr":            "aZ0 !\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~èñ日本語🔐 trailing ",
		"extra newline":          brutalPassphrase + "\n",
		"empty":                  "",
	} {
		if _, err := UnwrapKeys(bytes.NewReader(buf.Bytes()), Identity{Passphrase: []byte(wrong)}); err == nil {
			t.Fatalf("%s must not unwrap the key file", name)
		}
	}
}

func TestAssessPassphrase(t *testing.T) {
	cases := []struct {
		name string
		pass string
		weak bool
	}{
		{"empty", "", true},
		{"short", "hunter2", true},
		{"e2e style phrase", "phase06-e2e-passphrase", true},
		{"repeated", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"32 random with symbols", "#G(gj%nc|uWNEnPr4JzRSv)Riea^M:,=", false},
		{"32 random alnum", "CcSuwMWv28EGJjaKLPPWnsEK2ha85EV4", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := AssessPassphrase([]byte(c.pass))
			if a.Weak != c.weak {
				t.Fatalf("weak=%v, want %v (runes=%d classes=%d bits=%.0f)",
					a.Weak, c.weak, a.Runes, a.Classes, a.Bits)
			}
			if (a.Warning() != "") != c.weak {
				t.Fatalf("warning presence %v does not match weak=%v", a.Warning() != "", c.weak)
			}
			// The verdict must never leak the passphrase it judged.
			if c.pass != "" && bytes.Contains([]byte(a.Warning()), []byte(c.pass)) {
				t.Fatal("warning must never contain the passphrase")
			}
		})
	}
}

func TestAssessPassphraseCountsRunesNotBytes(t *testing.T) {
	a := AssessPassphrase([]byte("日本語"))
	if a.Runes != 3 {
		t.Fatalf("runes %d, want 3", a.Runes)
	}
	if a.Classes != 1 {
		t.Fatalf("classes %d, want 1 (non-ASCII only)", a.Classes)
	}
}
