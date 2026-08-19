package cli

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/spf13/cobra"

	"github.com/manprint/backimage/pkg/crypt"
)

// Character classes offered by genpass. Symbols are the printable ASCII
// punctuation except the space: a passphrase is passed around in files, shell
// variables and `docker run -e`, and a leading or trailing space is too easy to
// lose on the way.
const (
	genpassLower   = "abcdefghijkmnopqrstuvwxyz"
	genpassUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	genpassDigit   = "23456789"
	genpassSymbol  = "!#$%&()*+,-./:;<=>?@[]^_{|}~" //nolint:gosec // G101 falso positivo: alfabeto, non una credenziale.
	genpassLowerL  = "abcdefghijklmnopqrstuvwxyz"
	genpassUpperL  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	genpassDigitL  = "0123456789"
	genpassMinimum = 16
	genpassDefault = 32
)

// genpassClasses is one class of characters the result must contain.
type genpassClass struct {
	name  string
	chars string
}

// genpassAlphabet returns the classes to draw from.
//
// By default the ambiguous glyphs l, I, 1, O, 0 are left out: a key gets copied
// by hand and read off a screen, and a backup that cannot be unlocked because a
// 1 was read as an l is lost exactly like a forgotten passphrase. --unambiguous
// can be turned off to use the full ASCII alphabet.
func genpassAlphabet(symbols, ambiguous bool) []genpassClass {
	lower, upper, digit := genpassLower, genpassUpper, genpassDigit
	if ambiguous {
		lower, upper, digit = genpassLowerL, genpassUpperL, genpassDigitL
	}
	classes := []genpassClass{
		{"minuscole", lower},
		{"maiuscole", upper},
		{"cifre", digit},
	}
	if symbols {
		classes = append(classes, genpassClass{"simboli", genpassSymbol})
	}
	return classes
}

// randomIndex returns a uniform integer in [0, n) from crypto/rand. Using
// crypto/rand.Int keeps the draw free of the modulo bias a plain % would add.
func randomIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("crypto/rand: %w", err)
	}
	return int(v.Int64()), nil
}

// generatePassphrase draws length characters uniformly from the union of the
// given classes and guarantees at least one character of each.
//
// The guarantee is applied by drawing the whole string first and then, for any
// class left unused, replacing a character at a random position with one of
// that class. Positions are chosen without repetition, so the fix never
// overwrites a character it just placed. The loss of entropy is bounded by the
// number of classes and is far smaller than the gain in usability: a key that a
// password field rejects for missing a digit is a key nobody uses.
func generatePassphrase(length int, classes []genpassClass) (string, error) {
	if length < genpassMinimum {
		return "", fmt.Errorf("lunghezza %d troppo corta: minimo %d", length, genpassMinimum)
	}
	if len(classes) == 0 {
		return "", fmt.Errorf("nessuna classe di caratteri selezionata")
	}
	if length < len(classes) {
		return "", fmt.Errorf("lunghezza %d inferiore al numero di classi richieste (%d)", length, len(classes))
	}
	var alphabet strings.Builder
	for _, c := range classes {
		alphabet.WriteString(c.chars)
	}
	pool := alphabet.String()

	out := make([]byte, length)
	for i := range out {
		idx, err := randomIndex(len(pool))
		if err != nil {
			return "", err
		}
		out[i] = pool[idx]
	}

	// Which positions are still free to be overwritten by the class fix-up.
	free := make([]int, length)
	for i := range free {
		free[i] = i
	}
	for _, c := range classes {
		if strings.ContainsAny(string(out), c.chars) {
			continue
		}
		if len(free) == 0 {
			return "", fmt.Errorf("lunghezza %d insufficiente per coprire tutte le classi", length)
		}
		slot, err := randomIndex(len(free))
		if err != nil {
			return "", err
		}
		pos := free[slot]
		free = append(free[:slot], free[slot+1:]...)
		pick, err := randomIndex(len(c.chars))
		if err != nil {
			return "", err
		}
		out[pos] = c.chars[pick]
	}
	return string(out), nil
}

type genpassResult struct {
	Passphrase string  `json:"passphrase"`
	Length     int     `json:"length"`
	Alphabet   int     `json:"alphabet"`
	Bits       float64 `json:"bits"`
}

func newGenpassCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "genpass",
		Short: "generate a strong passphrase for a backup",
		Long: "generate a strong passphrase for a backup.\n\n" +
			"Characters are drawn from crypto/rand without modulo bias, and the\n" +
			"result contains at least one character of every selected class. The\n" +
			"default is 32 characters over lowercase, uppercase, digits and\n" +
			"symbols, which is far past the point where the passphrase stops being\n" +
			"the weakest part of the backup.\n\n" +
			"The passphrase is printed on stdout and nowhere else: it is never\n" +
			"logged, stored or sent to a registry. There is no recovery path, so\n" +
			"save it before making a backup with it.\n\n" +
			"Ambiguous glyphs (l, I, 1, O, 0) are excluded by default so a key can\n" +
			"be read off a screen without losing the backup to a misread character.\n\n" +
			"Examples:\n" +
			"  backimage genpass\n" +
			"  backimage genpass --length 48\n" +
			"  backimage genpass --count 5\n" +
			"  backimage genpass --no-symbols --ambiguous\n" +
			"  backimage genpass > key.txt && chmod 600 key.txt\n" +
			"  backimage genpass | tee key.txt | backimage backup /data --repo R --passphrase-stdin",
		Args: cobra.NoArgs,
		RunE: runGenpass,
	}
	cmd.Flags().Int("length", genpassDefault,
		fmt.Sprintf("number of characters (minimum %d)", genpassMinimum))
	cmd.Flags().Int("count", 1, "how many passphrases to generate")
	cmd.Flags().Bool("no-symbols", false, "letters and digits only, for fields that reject punctuation")
	cmd.Flags().Bool("ambiguous", false, "also use the look-alike characters l, I, 1, O and 0")
	return cmd
}

func runGenpass(cmd *cobra.Command, _ []string) error {
	opts, err := parseOptions(cmd.Root())
	if err != nil {
		return err
	}
	length := getFlagInt(cmd, "length")
	count := getFlagInt(cmd, "count")
	if count < 1 {
		return New(KindUsage, "", "--count deve essere almeno 1")
	}
	classes := genpassAlphabet(!getFlagBool(cmd, "no-symbols"), getFlagBool(cmd, "ambiguous"))

	results := make([]genpassResult, 0, count)
	alphabet := 0
	for _, c := range classes {
		alphabet += len(c.chars)
	}
	for i := 0; i < count; i++ {
		pass, err := generatePassphrase(length, classes)
		if err != nil {
			return New(KindUsage, "", "%v", err)
		}
		results = append(results, genpassResult{
			Passphrase: pass,
			Length:     len([]rune(pass)),
			Alphabet:   alphabet,
			Bits:       crypt.AssessPassphrase([]byte(pass)).Bits,
		})
	}

	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if opts.JSON {
		if count == 1 {
			return printerResult(pr, results[0])
		}
		return printerResult(pr, results)
	}
	for _, r := range results {
		if err := printerResult(pr, r.Passphrase); err != nil {
			return err
		}
	}
	return nil
}
