package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/manprint/backimage/pkg/crypt"
)

// rewrapMockKeys seals the mock backup's key file under passphrase instead of
// cliRestorePass.
func rewrapMockKeys(t *testing.T, s *mockImageSource, passphrase string) {
	t.Helper()
	km, err := crypt.UnwrapKeys(bytes.NewReader(s.keys["keys.pass.age"]), crypt.Identity{Passphrase: []byte(cliRestorePass)})
	if err != nil {
		t.Fatal(err)
	}
	defer km.Wipe()
	var wrapped bytes.Buffer
	if err := crypt.WrapKeys(&wrapped, km, crypt.Recipients{Passphrase: []byte(passphrase)}); err != nil {
		t.Fatal(err)
	}
	s.keys["keys.pass.age"] = wrapped.Bytes()
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

// The same line is the same passphrase whichever way it arrives. A backup a
// cron job sealed from a file had to be opened from a file: typed into stdin
// with a CRLF line ending, the line kept its \r and was a wrong passphrase.
func TestRestoreOpensABackupFromAnotherPassphraseSource(t *testing.T) {
	s, _ := newMockImageSource(t, true)
	withMockSource(t, s)
	t.Setenv("BACKIMAGE_PASSPHRASE", "")
	stdinFrom(t, cliRestorePass+"\r\n")
	if _, stderr, err := runRoot(t, "restore", "example.test/repo:tag", "-o", filepath.Join(t.TempDir(), "out.tar"),
		"--passphrase-stdin"); err != nil {
		t.Fatalf("restore with the passphrase on a CRLF stdin line: %v\n%s", err, stderr)
	}
}

// Backups sealed under the earlier reading of a file stay readable from that
// same file: "pass\n\n" used to be the passphrase "pass\n".
func TestRestoreOpensABackupSealedUnderTheEarlierFileReading(t *testing.T) {
	s, _ := newMockImageSource(t, true)
	rewrapMockKeys(t, s, cliRestorePass+"\n")
	withMockSource(t, s)
	t.Setenv("BACKIMAGE_PASSPHRASE", "")
	passFile := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(passFile, []byte(cliRestorePass+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runRoot(t, "restore", "example.test/repo:tag", "-o", filepath.Join(t.TempDir(), "out.tar"),
		"--passphrase-file", passFile); err != nil {
		t.Fatalf("restore of a backup sealed under the earlier reading: %v\n%s", err, stderr)
	}
	// A wrong passphrase is still a wrong passphrase after every reading.
	if err := os.WriteFile(passFile, []byte("wrong\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, _ := newMockImageSource(t, true)
	withMockSource(t, s2)
	_, _, err := runRoot(t, "restore", "example.test/repo:tag", "-o", filepath.Join(t.TempDir(), "bad.tar"),
		"--passphrase-file", passFile)
	if ExitCodeFor(err) != int(KindPassphrase) {
		t.Fatalf("wrong passphrase exit = %d (%v), want %d", ExitCodeFor(err), err, KindPassphrase)
	}
}

// An empty BACKIMAGE_PASSPHRASE is an unset one. It used to count as a
// credential, so restoring an unencrypted backup failed as a suspected
// substitution.
func TestEmptyPassphraseEnvIsNotACredential(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	t.Setenv("BACKIMAGE_PASSPHRASE", "")
	if _, stderr, err := runRoot(t, "restore", "example.test/repo:tag", "-o", filepath.Join(t.TempDir(), "out.tar")); err != nil {
		t.Fatalf("unencrypted restore with an empty BACKIMAGE_PASSPHRASE: %v\n%s", err, stderr)
	}
}
