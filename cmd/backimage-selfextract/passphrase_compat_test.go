package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/manprint/backimage/pkg/crypt"
)

func rewrapFixtureKeys(t *testing.T, f commandFixture, passphrase string) {
	t.Helper()
	path := filepath.Join(f.root, "keys.pass.age")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	km, err := crypt.UnwrapKeys(bytes.NewReader(data), crypt.Identity{Passphrase: []byte(commandTestPass)})
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()
	writeCommandFile(t, path, func(w io.Writer) error {
		return crypt.WrapKeys(w, km, crypt.Recipients{Passphrase: []byte(passphrase)})
	})
}

func stdinFrom(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
	})
	go func() {
		w.Write([]byte(content))
		w.Close()
	}()
}

// Same rule as the host binary: one line is one passphrase from any source.
func TestExtractOpensABackupFromAnotherPassphraseSource(t *testing.T) {
	f := newCommandFixture(t, true)
	t.Setenv("BACKIMAGE_PASSPHRASE", "")
	stdinFrom(t, commandTestPass+"\r\n")
	if out, stderr, err := captureRun(t, "list", "--root", f.root, "--passphrase-stdin"); err != nil {
		t.Fatalf("list with the passphrase on a CRLF stdin line: %q %v\n%s", out, err, stderr)
	}
}

func TestExtractOpensABackupSealedUnderTheEarlierFileReading(t *testing.T) {
	f := newCommandFixture(t, true)
	rewrapFixtureKeys(t, f, commandTestPass+"\n")
	t.Setenv("BACKIMAGE_PASSPHRASE", "")
	passFile := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(passFile, []byte(commandTestPass+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, stderr, err := captureRun(t, "list", "--root", f.root, "--passphrase-file", passFile); err != nil {
		t.Fatalf("list of a backup sealed under the earlier reading: %q %v\n%s", out, err, stderr)
	}
	if err := os.WriteFile(passFile, []byte("wrong\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := captureRun(t, "list", "--root", f.root, "--passphrase-file", passFile); exitCode(err) != exitPassphrase {
		t.Fatalf("wrong passphrase exit = %d (%v), want %d", exitCode(err), err, exitPassphrase)
	}
}

func TestEmptyPassphraseEnvIsNotACredential(t *testing.T) {
	f := newCommandFixture(t, false)
	t.Setenv("BACKIMAGE_PASSPHRASE", "")
	if out, _, err := captureRun(t, "list", "--root", f.root); err != nil {
		t.Fatalf("unencrypted list with an empty BACKIMAGE_PASSPHRASE: %q %v", out, err)
	}
}
