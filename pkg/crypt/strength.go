package crypt

import (
	"fmt"
	"math"
	"unicode"
	"unicode/utf8"
)

// MinRecommendedBits is the guessing work, in bits, below which a passphrase is
// reported as weak.
//
// The wrapped key is protected by age's scrypt at N=2^18, r=8, p=1: roughly
// 256 MiB and a second of CPU per guess, which is a large but finite constant.
// Above ~96 bits of real entropy no amount of hardware closes the gap, and the
// passphrase stops being the weakest part of the backup.
const MinRecommendedBits = 96

// PassphraseAssessment describes a passphrase without ever holding it.
//
// Bits is the entropy the passphrase would have if every character had been
// drawn at random from the character classes it uses. For a generated key that
// is the real figure. For a phrase somebody made up and can remember it is a
// wild overestimate — natural language carries on the order of 1 to 2 bits per
// character, so a 24-character sentence is worth around 30 bits, not the 150
// the arithmetic suggests. Treat Bits as a ceiling, never as a guarantee.
type PassphraseAssessment struct {
	// Runes is the length in characters, not bytes.
	Runes int
	// Classes counts the character classes used: lowercase, uppercase, digits,
	// symbols, and anything outside ASCII.
	Classes int
	// Distinct is the number of distinct characters.
	Distinct int
	// Bits is the upper bound on guessing work described above.
	Bits float64
	// Weak is true when Bits falls below MinRecommendedBits.
	Weak bool
}

// AssessPassphrase measures p. It never copies, logs or returns the passphrase
// itself, so the result is safe to print.
func AssessPassphrase(p []byte) PassphraseAssessment {
	var (
		a        PassphraseAssessment
		lower    bool
		upper    bool
		digit    bool
		symbol   bool
		nonASCII bool
	)
	distinct := make(map[rune]struct{})
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRune(p[i:])
		i += size
		a.Runes++
		distinct[r] = struct{}{}
		switch {
		case r > unicode.MaxASCII || r == utf8.RuneError:
			nonASCII = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	a.Distinct = len(distinct)

	// Charset size per class, deliberately conservative: 26+26+10 for the
	// ASCII letter and digit classes, 33 printable ASCII symbols, and a token
	// 100 for everything beyond ASCII, which no attacker would enumerate in
	// full anyway.
	space := 0
	for _, c := range []struct {
		used bool
		size int
	}{{lower, 26}, {upper, 26}, {digit, 10}, {symbol, 33}, {nonASCII, 100}} {
		if c.used {
			space += c.size
			a.Classes++
		}
	}
	if a.Runes > 0 && space > 1 {
		a.Bits = float64(a.Runes) * math.Log2(float64(space))
	}
	// A passphrase that repeats a handful of characters has far less entropy
	// than its length claims. Scale the estimate by the share of distinct
	// characters so "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" is not called strong.
	if a.Runes > 0 {
		a.Bits *= float64(a.Distinct) / float64(a.Runes)
	}
	a.Weak = a.Bits < MinRecommendedBits
	return a
}

// Warning returns the message to show for a weak passphrase, or the empty
// string when there is nothing to say. It never contains the passphrase.
func (a PassphraseAssessment) Warning() string {
	if !a.Weak {
		return ""
	}
	return fmt.Sprintf(
		"attenzione: passphrase debole (%d caratteri, %d classi, al massimo ~%.0f bit; "+
			"consigliati %d). Chi possiede l'immagine puo' provare le passphrase offline, "+
			"senza limiti di tentativi: e' l'unica difesa del backup. "+
			"La stima assume caratteri scelti a caso; una frase inventata vale molto meno. "+
			"Generare una chiave con `backimage genpass`",
		a.Runes, a.Classes, a.Bits, MinRecommendedBits)
}
