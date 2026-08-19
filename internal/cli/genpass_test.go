package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/manprint/backimage/pkg/crypt"
)

func runGenpassCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := NewRootCommand()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestGenpassDefaults(t *testing.T) {
	out, _, err := runGenpassCmd(t, "genpass")
	if err != nil {
		t.Fatal(err)
	}
	pass := strings.TrimRight(out, "\n")
	if strings.Contains(pass, "\n") {
		t.Fatalf("one passphrase must be one line: %q", out)
	}
	if len([]rune(pass)) != genpassDefault {
		t.Fatalf("length %d, want %d", len([]rune(pass)), genpassDefault)
	}
	// The default must clear the strength bar the backup command warns about,
	// or genpass would be recommending something it flags.
	if a := crypt.AssessPassphrase([]byte(pass)); a.Weak {
		t.Fatalf("the default genpass output must not be weak: %.0f bits", a.Bits)
	}
}

// TestGenpassCoversEveryClass exercises the guarantee at the tightest length,
// where the fewest spare positions exist. The first implementation drew the
// whole string from the union and then overwrote a position per missing class,
// which could land on the sole character satisfying another class and destroy
// it; that failed here roughly once every few hundred draws. The guarantee now
// holds by construction, so any failure is a real regression and not a bad roll.
func TestGenpassCoversEveryClass(t *testing.T) {
	classes := genpassAlphabet(true, false)
	for i := 0; i < 500; i++ {
		pass, err := generatePassphrase(genpassMinimum, classes)
		if err != nil {
			t.Fatal(err)
		}
		for _, class := range classes {
			if !strings.ContainsAny(pass, class.chars) {
				t.Fatalf("draw %d has no %s: %q", i, class.name, pass)
			}
		}
	}
	// The same through the command, including the default length.
	for i := 0; i < 50; i++ {
		out, _, err := runGenpassCmd(t, "genpass", "--length", "16")
		if err != nil {
			t.Fatal(err)
		}
		pass := strings.TrimRight(out, "\n")
		for _, class := range classes {
			if !strings.ContainsAny(pass, class.chars) {
				t.Fatalf("command draw %d has no %s: %q", i, class.name, pass)
			}
		}
	}
}

// TestGenpassShufflesClassRepresentatives guards the other half of the
// construction. One character per class is drawn first, so without the shuffle
// position 0 would always be a lowercase letter, position 1 always uppercase,
// and so on for every key this command prints — a structure worth far more to an
// attacker than the class guarantee is worth to the user.
func TestGenpassShufflesClassRepresentatives(t *testing.T) {
	classes := genpassAlphabet(true, false)
	firstIsLower := 0
	const draws = 300
	for i := 0; i < draws; i++ {
		pass, err := generatePassphrase(32, classes)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(pass[:1], classes[0].chars) {
			firstIsLower++
		}
	}
	// Lowercase is 25 of the 85 characters in the default alphabet, so an
	// unbiased first position lands there about 29% of the time. Without the
	// shuffle it would be 100%.
	if firstIsLower > draws*3/4 {
		t.Fatalf("first character is a lowercase letter in %d/%d draws: the shuffle is not running",
			firstIsLower, draws)
	}
}

func TestGenpassExcludesAmbiguousByDefault(t *testing.T) {
	const ambiguous = "lI1O0"
	for i := 0; i < 100; i++ {
		out, _, err := runGenpassCmd(t, "genpass", "--length", "64")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimRight(out, "\n"); strings.ContainsAny(got, ambiguous) {
			t.Fatalf("draw %d contains a look-alike character: %q", i, got)
		}
	}
	// --ambiguous puts them back: over 100 draws of 64 characters the odds of
	// never seeing one are negligible.
	var seen bool
	for i := 0; i < 100 && !seen; i++ {
		out, _, err := runGenpassCmd(t, "genpass", "--length", "64", "--ambiguous")
		if err != nil {
			t.Fatal(err)
		}
		seen = strings.ContainsAny(strings.TrimRight(out, "\n"), ambiguous)
	}
	if !seen {
		t.Fatal("--ambiguous never produced a look-alike character")
	}
}

func TestGenpassNoSymbols(t *testing.T) {
	out, _, err := runGenpassCmd(t, "genpass", "--no-symbols", "--length", "40")
	if err != nil {
		t.Fatal(err)
	}
	pass := strings.TrimRight(out, "\n")
	if strings.ContainsAny(pass, genpassSymbol) {
		t.Fatalf("--no-symbols still produced punctuation: %q", pass)
	}
	if len([]rune(pass)) != 40 {
		t.Fatalf("length %d, want 40", len([]rune(pass)))
	}
}

func TestGenpassCountAndUniqueness(t *testing.T) {
	out, _, err := runGenpassCmd(t, "genpass", "--count", "50")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if seen[l] {
			t.Fatalf("genpass repeated a passphrase: %q", l)
		}
		seen[l] = true
	}
}

func TestGenpassJSON(t *testing.T) {
	out, _, err := runGenpassCmd(t, "--json", "genpass", "--length", "24")
	if err != nil {
		t.Fatal(err)
	}
	var r genpassResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if r.Length != 24 || len([]rune(r.Passphrase)) != 24 {
		t.Fatalf("length mismatch: %+v", r)
	}
	if r.Bits <= 0 || r.Alphabet <= 0 {
		t.Fatalf("missing strength fields: %+v", r)
	}

	out, _, err = runGenpassCmd(t, "--json", "genpass", "--count", "3")
	if err != nil {
		t.Fatal(err)
	}
	var list []genpassResult
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("invalid JSON array %q: %v", out, err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d results, want 3", len(list))
	}
}

func TestGenpassRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"genpass", "--length", "4"},
		{"genpass", "--length", "0"},
		{"genpass", "--count", "0"},
		{"genpass", "extra-arg"},
	} {
		if _, _, err := runGenpassCmd(t, args...); err == nil {
			t.Fatalf("%v must fail", args)
		}
	}
}

// TestGenpassDistribution is a coarse bias check: over many draws every
// character of the alphabet should appear. A modulo-biased or truncated draw
// leaves part of the alphabet unreachable, which this catches.
func TestGenpassDistribution(t *testing.T) {
	classes := genpassAlphabet(true, false)
	var alphabet strings.Builder
	for _, c := range classes {
		alphabet.WriteString(c.chars)
	}
	seen := map[rune]int{}
	for i := 0; i < 400; i++ {
		pass, err := generatePassphrase(64, classes)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pass {
			seen[r]++
		}
	}
	for _, r := range alphabet.String() {
		if seen[r] == 0 {
			t.Fatalf("character %q never drawn: the alphabet is not uniformly reachable", r)
		}
	}
}

func TestGeneratePassphraseGuarantees(t *testing.T) {
	if _, err := generatePassphrase(genpassMinimum-1, genpassAlphabet(true, false)); err == nil {
		t.Fatal("below the minimum length must fail")
	}
	if _, err := generatePassphrase(32, nil); err == nil {
		t.Fatal("no character class must fail")
	}
}
