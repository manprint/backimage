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
	File    string // --passphrase-file
	Stdin   bool   // --passphrase-stdin (reads one line)
	EnvVar  string // default "BACKIMAGE_PASSPHRASE"
	Prompt  bool   // interactive prompt on /dev/tty
	Confirm bool   // ask twice and compare (backup only)

	// openTTY is injectable in tests; defaults to opening /dev/tty.
	openTTY func() (io.ReadWriteCloser, error)
}

// ErrEmptyPassphrase is returned when the resolved source yields zero bytes.
var ErrEmptyPassphrase = errors.New("empty passphrase")

// ReadPassphrase resolves a passphrase according to src.
func ReadPassphrase(src PassphraseSource) ([]byte, error) {
	if src.openTTY == nil {
		src.openTTY = openDevTTY
	}
	if src.Direct != nil {
		if len(src.Direct) == 0 {
			return nil, ErrEmptyPassphrase
		}
		return append([]byte(nil), src.Direct...), nil
	}
	if src.File != "" {
		data, err := os.ReadFile(src.File)
		if err != nil {
			return nil, fmt.Errorf("passphrase file: %w", err)
		}
		line := string(data)
		if strings.HasSuffix(line, "\r\n") {
			line = line[:len(line)-2]
		} else if strings.HasSuffix(line, "\n") {
			line = line[:len(line)-1]
		}
		if line == "" {
			return nil, ErrEmptyPassphrase
		}
		return []byte(line), nil
	}
	if src.Stdin {
		var buf strings.Builder
		b := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(b)
			if n > 0 {
				if b[0] == '\n' {
					break
				}
				buf.WriteByte(b[0])
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
		}
		if buf.Len() == 0 {
			return nil, ErrEmptyPassphrase
		}
		return []byte(buf.String()), nil
	}
	env := src.EnvVar
	if env == "" {
		env = "BACKIMAGE_PASSPHRASE"
	}
	if v, ok := os.LookupEnv(env); ok {
		if v == "" {
			return nil, ErrEmptyPassphrase
		}
		return []byte(v), nil
	}
	if src.Prompt {
		tty, err := src.openTTY()
		if err != nil {
			return nil, fmt.Errorf("%w: cannot open /dev/tty, use --passphrase-file or --passphrase-stdin", ErrNoPassphrase)
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
				return nil, err
			}
			fmt.Fprintln(tty)
			if len(first) == 0 {
				return nil, ErrEmptyPassphrase
			}
			if src.Confirm {
				fmt.Fprint(tty, "Conferma: ")
				second, err := readPassword(tty)
				fmt.Fprintln(tty)
				if err != nil {
					return nil, err
				}
				if subtle.ConstantTimeCompare(first, second) != 1 {
					casts++
					zero(first)
					zero(second)
					if casts >= 3 {
						return nil, errors.New("passphrase confirm failed after 3 attempts")
					}
					continue
				}
				zero(second)
			}
			return first, nil
		}
	}
	return nil, ErrNoPassphrase
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
