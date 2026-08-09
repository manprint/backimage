package crypt

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type mockTTY struct {
	in  io.Reader
	out bytes.Buffer
}

func (m *mockTTY) Read(p []byte) (int, error)  { return m.in.Read(p) }
func (m *mockTTY) Write(p []byte) (int, error) { return m.out.Write(p) }
func (m *mockTTY) Close() error                { return nil }

func mockTTYFactory(lines ...string) func() (io.ReadWriteCloser, error) {
	joined := strings.Join(lines, "\n") + "\n"
	return func() (io.ReadWriteCloser, error) {
		return &mockTTY{in: bytes.NewBufferString(joined)}, nil
	}
}

func TestPassphraseDirect(t *testing.T) {
	p, err := ReadPassphrase(PassphraseSource{Direct: []byte("p")})
	if err != nil || string(p) != "p" {
		t.Fatalf("direct: %q %v", p, err)
	}
	if _, err := ReadPassphrase(PassphraseSource{Direct: []byte{}}); !errors.Is(err, ErrEmptyPassphrase) {
		t.Fatalf("empty direct: %v", err)
	}
}

func TestPassphraseFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.txt")
	os.WriteFile(f, []byte("filepass\n"), 0o600)
	p, err := ReadPassphrase(PassphraseSource{File: f})
	if err != nil || string(p) != "filepass" {
		t.Fatalf("file: %q %v", p, err)
	}
	os.WriteFile(f, []byte("crlfpass\r\n"), 0o600)
	p, err = ReadPassphrase(PassphraseSource{File: f})
	if err != nil || string(p) != "crlfpass" {
		t.Fatalf("crlf file: %q %v", p, err)
	}
	os.WriteFile(f, []byte("\n"), 0o600)
	if _, err := ReadPassphrase(PassphraseSource{File: f}); !errors.Is(err, ErrEmptyPassphrase) {
		t.Fatalf("empty file: %v", err)
	}
	if _, err := ReadPassphrase(PassphraseSource{File: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestPassphraseStdin(t *testing.T) {
	old := os.Stdin
	os.Stdin, _ = os.Open("/dev/null")
	defer func() { os.Stdin = old }()
	// simulate via a real pipe-free path: invoke the same loop by swapping
	// through a helper is overkill; Stdin branch reads os.Stdin directly,
	// so instead cover it with a redirected file descriptor.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	w.Write([]byte("stdinpass\n"))
	w.Close()
	p, err := ReadPassphrase(PassphraseSource{Stdin: true})
	if err != nil || string(p) != "stdinpass" {
		t.Fatalf("stdin: %q %v", p, err)
	}
	os.Stdin = old
}

func TestPassphraseEnv(t *testing.T) {
	t.Setenv("BACKIMAGE_PASSPHRASE", "envval")
	p, err := ReadPassphrase(PassphraseSource{EnvVar: "BACKIMAGE_PASSPHRASE"})
	if err != nil || string(p) != "envval" {
		t.Fatalf("env: %q %v", p, err)
	}
	t.Setenv("BACKIMAGE_PASSPHRASE", "")
	if _, err := ReadPassphrase(PassphraseSource{EnvVar: "BACKIMAGE_PASSPHRASE"}); !errors.Is(err, ErrEmptyPassphrase) {
		t.Fatalf("empty env: %v", err)
	}
	t.Setenv("BACKIMAGE_PASSPHRASE", "x")
	if p, err := ReadPassphrase(PassphraseSource{}); err != nil || string(p) != "x" {
		t.Fatalf("default env var must be consulted: %q %v", p, err)
	}
}

func TestPassphraseNoSource(t *testing.T) {
	if _, err := ReadPassphrase(PassphraseSource{}); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("no source must yield ErrNoPassphrase, got %v", err)
	}
}

func TestPassphrasePromptSingle(t *testing.T) {
	src := PassphraseSource{Prompt: true, openTTY: mockTTYFactory("secret")}
	p, err := ReadPassphrase(src)
	if err != nil || string(p) != "secret" {
		t.Fatalf("prompt: %q %v", p, err)
	}
}

func TestPassphrasePromptConfirmMismatch(t *testing.T) {
	src := PassphraseSource{Prompt: true, Confirm: true, openTTY: mockTTYFactory("a", "b", "a", "b", "c", "c")}
	p, err := ReadPassphrase(src)
	if err != nil || string(p) != "c" {
		t.Fatalf("confirm retry: %q %v", p, err)
	}
	src = PassphraseSource{Prompt: true, Confirm: true, openTTY: mockTTYFactory("x", "y", "x", "y", "x", "y")}
	if _, err := ReadPassphrase(src); err == nil || !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("3 mismatches must abort, got %v", err)
	}
}

func TestPassphrasePromptNoTTY(t *testing.T) {
	src := PassphraseSource{Prompt: true, openTTY: func() (io.ReadWriteCloser, error) {
		return nil, errors.New("no tty")
	}}
	if _, err := ReadPassphrase(src); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("tty failure must map to ErrNoPassphrase, got %v", err)
	}
}

func TestPassphrasePromptEmpty(t *testing.T) {
	src := PassphraseSource{Prompt: true, openTTY: mockTTYFactory("")}
	if _, err := ReadPassphrase(src); !errors.Is(err, ErrEmptyPassphrase) {
		t.Fatalf("empty prompt answer: %v", err)
	}
}

func TestPriorityDirectOverEnv(t *testing.T) {
	t.Setenv("BACKIMAGE_PASSPHRASE", "env")
	p, err := ReadPassphrase(PassphraseSource{Direct: []byte("d")})
	if err != nil || string(p) != "d" {
		t.Fatalf("direct must win: %q %v", p, err)
	}
}

func TestOpenDevTTY(t *testing.T) {
	if tty, err := openDevTTY(); err == nil {
		if _, ok := tty.(*os.File); !ok {
			t.Fatal("openDevTTY must return *os.File")
		}
		tty.Close()
	}
	// deterministic coverage without a controlling terminal: a plain file
	// succeeds, a missing path fails.
	f, err := openTTYAt("/dev/null")
	if err != nil || f == nil {
		t.Fatalf("openTTYAt(/dev/null): %v", err)
	}
	f.Close()
	if _, err := openTTYAt("/definitely/not/a/tty"); err == nil {
		t.Fatal("missing tty path must error")
	}
}
