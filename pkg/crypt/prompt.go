package crypt

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ErrNoPassphrase is returned when no source yields a passphrase.
var ErrNoPassphrase = errors.New("no passphrase available")

// PassphraseSource describes where a passphrase may come from, in priority order.
type PassphraseSource struct {
	Direct  []byte // already provided by the caller (for example --password)
	File    string // --passphrase-file (its first line)
	Stdin   bool   // --passphrase-stdin (reads one line)
	EnvVar  string // default "BACKIMAGE_PASSPHRASE"; empty counts as unset
	Prompt  bool   // interactive prompt on /dev/tty
	Confirm bool   // ask twice and compare (backup only)

	// Warn, when set, receives a notice about the input that does not
	// carry any of its content: a passphrase file with more than one line.
	Warn func(string)

	// openTTY is injectable in tests; defaults to opening /dev/tty.
	openTTY func() (io.ReadWriteCloser, error)
}

// ErrEmptyPassphrase is returned when the resolved source yields zero bytes.
var ErrEmptyPassphrase = errors.New("empty passphrase")

// ReadPassphrase resolves a passphrase according to src.
//
// A file and stdin are read by one rule: the first line, without its line
// break (\n or \r\n). A passphrase is a line of text, so the same text is the
// same passphrase whichever way it arrives — the prompt, which never returns
// a line break, included.
func ReadPassphrase(src PassphraseSource) ([]byte, error) {
	current, legacy, err := readPassphrase(src)
	for _, p := range legacy {
		zero(p)
	}
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return nil, ErrEmptyPassphrase
	}
	return current, nil
}

// ReadPassphraseAll resolves src like ReadPassphrase and also returns the
// readings releases up to 0.6.1 made of the same input, where they differ
// from it: the whole file but one final line break, and the stdin line with
// its \r. A backup sealed under one of those must stay readable from the
// input that sealed it. A backup is always sealed under the passphrase; the
// earlier readings only ever open one. The caller wipes all of them.
func ReadPassphraseAll(src PassphraseSource) (passphrase []byte, earlier [][]byte, err error) {
	current, legacy, err := readPassphrase(src)
	if err != nil {
		return nil, nil, err
	}
	if len(current) == 0 {
		for _, p := range legacy {
			zero(p)
		}
		return nil, nil, ErrEmptyPassphrase
	}
	return current, legacy, nil
}

// ReadPassphraseCandidates is for readers: the passphrases to try on a backup,
// in order — the current reading of src, then the earlier readings
// ReadPassphraseAll describes. A file whose first line is empty has no
// current reading but may still hold the passphrase an earlier release read.
// The caller wipes every candidate.
func ReadPassphraseCandidates(src PassphraseSource) ([][]byte, error) {
	current, legacy, err := readPassphrase(src)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	if len(current) > 0 {
		out = append(out, current)
	}
	out = append(out, legacy...)
	if len(out) == 0 {
		return nil, ErrEmptyPassphrase
	}
	return out, nil
}

// PassphraseEnvSet reports whether the environment variable env (default
// BACKIMAGE_PASSPHRASE) holds a passphrase. An empty value is an unset one.
func PassphraseEnvSet(env string) bool {
	if env == "" {
		env = "BACKIMAGE_PASSPHRASE"
	}
	return os.Getenv(env) != ""
}

// passphraseLine is the one rule for text input: the first line, without its
// line break.
func passphraseLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimSuffix(line, "\r")
}

// readPassphrase returns the passphrase src yields under the current rule,
// which may be empty, and the earlier readings of the same input that differ
// from it.
func readPassphrase(src PassphraseSource) (current []byte, legacy [][]byte, err error) {
	if src.openTTY == nil {
		src.openTTY = openDevTTY
	}
	if src.Direct != nil {
		if len(src.Direct) == 0 {
			return nil, nil, ErrEmptyPassphrase
		}
		return append([]byte(nil), src.Direct...), nil, nil
	}
	if src.File != "" {
		data, err := os.ReadFile(src.File)
		if err != nil {
			return nil, nil, fmt.Errorf("passphrase file: %w", err)
		}
		text := string(data)
		zero(data)
		line := passphraseLine(text)
		if rest := text[min(len(line)+1, len(text)):]; strings.Trim(rest, "\r\n") != "" && src.Warn != nil {
			src.Warn("passphrase file: contiene più di una riga, si usa soltanto la prima")
		}
		// Up to 0.6.1: the whole file, less one final \n or \r\n.
		old := strings.TrimSuffix(text, "\n")
		if len(old) < len(text) {
			old = strings.TrimSuffix(old, "\r")
		}
		if old != line && old != "" {
			legacy = append(legacy, []byte(old))
		}
		return []byte(line), legacy, nil
	}
	if src.Stdin {
		var buf []byte
		b := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(b)
			if n > 0 {
				if b[0] == '\n' {
					break
				}
				buf = append(buf, b[0])
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				zero(buf)
				return nil, nil, err
			}
		}
		line := passphraseLine(string(buf))
		// Up to 0.6.1: the line as read, a final \r included.
		if len(line) < len(buf) {
			legacy = append(legacy, append([]byte(nil), buf...))
		}
		zero(buf)
		if line == "" && len(legacy) == 0 {
			return nil, nil, ErrEmptyPassphrase
		}
		return []byte(line), legacy, nil
	}
	if PassphraseEnvSet(src.EnvVar) {
		env := src.EnvVar
		if env == "" {
			env = "BACKIMAGE_PASSPHRASE"
		}
		return []byte(os.Getenv(env)), nil, nil
	}
	if src.Prompt {
		tty, err := src.openTTY()
		if err != nil {
			return nil, nil, fmt.Errorf("%w: cannot open /dev/tty, use --passphrase-file or --passphrase-stdin", ErrNoPassphrase)
		}
		defer tty.Close()
		casts := 0
		for {
			if casts > 0 {
				fmt.Fprint(tty, "passphrase non coincidenti, riprova\n")
			}
			fmt.Fprint(tty, "Passphrase: ")
			first, err := readPassword(tty)
			if err != nil {
				return nil, nil, err
			}
			fmt.Fprintln(tty)
			if len(first) == 0 {
				return nil, nil, ErrEmptyPassphrase
			}
			if src.Confirm {
				fmt.Fprint(tty, "Conferma: ")
				second, err := readPassword(tty)
				fmt.Fprintln(tty)
				if err != nil {
					return nil, nil, err
				}
				if subtle.ConstantTimeCompare(first, second) != 1 {
					casts++
					zero(first)
					zero(second)
					if casts >= 3 {
						return nil, nil, errors.New("passphrase confirm failed after 3 attempts")
					}
					continue
				}
				zero(second)
			}
			return first, nil, nil
		}
	}
	return nil, nil, ErrNoPassphrase
}

// readPassword reads one line from tty. When tty is a real *os.File the
// echo is disabled via x/term; test doubles read the line as-is.
func readPassword(tty io.ReadWriteCloser) ([]byte, error) {
	if f, ok := tty.(*os.File); ok {
		return term.ReadPassword(int(f.Fd()))
	}
	// Test doubles: read a line as-is (echo cannot be disabled without a
	// real terminal).
	var out []byte
	buf := make([]byte, 1)
	for {
		n, err := tty.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return out, nil
			}
			out = append(out, buf[0])
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// openDevTTY opens the controlling terminal. On Windows this would open
// CONIN$, which is out of scope (unix builds only).
func openDevTTY() (io.ReadWriteCloser, error) {
	return openTTYAt("/dev/tty")
}

// openTTYAt opens ttyPath. Split out for deterministic tests (a real
// controlling terminal is not guaranteed on CI).
func openTTYAt(ttyPath string) (io.ReadWriteCloser, error) {
	return os.OpenFile(ttyPath, os.O_RDWR, 0)
}

// ErrPassphrase is a sentinel for usage-code mapping (exit 4).
var ErrPassphrase = errors.New("missing or wrong passphrase")
