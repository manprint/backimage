package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A passphrase says what the caller believes the backup to be. The strict
// opener stops a downgrade inside an encrypted backup; it cannot stop the
// whole backup from being replaced by a plaintext one, and that substitution
// used to restore attacker-controlled files while reporting success, with the
// supplied passphrase silently unused.

func TestUnencryptedBackupWithAPassphraseIsRefused(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	dir := t.TempDir()
	passFile := filepath.Join(dir, "pass")
	if err := os.WriteFile(passFile, []byte(cliRestorePass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"restore", "example.test/repo:tag", "-o", filepath.Join(dir, "out.tar"), "--passphrase-file", passFile},
		{"verify", "example.test/repo:tag", "--passphrase-file", passFile},
		{"ls", "example.test/repo:tag", "--passphrase-file", passFile},
		{"find", "example.test/repo:tag", "**/file.txt", "--passphrase-file", passFile},
	} {
		t.Run(args[0], func(t *testing.T) {
			_, _, err := runRoot(t, args...)
			if err == nil {
				t.Fatal("a credential on an unencrypted backup must not be ignored")
			}
			var cliErr *Error
			if !errors.As(err, &cliErr) || cliErr.Kind != KindIntegrity {
				t.Fatalf("want an integrity error (exit 5), got %#v", err)
			}
			if !strings.Contains(err.Error(), "--allow-unencrypted") &&
				!strings.Contains(cliErr.Hint, "--allow-unencrypted") {
				t.Fatalf("the error must name the way out, got %q / %q", err, cliErr.Hint)
			}
		})
	}
}

func TestUnencryptedBackupWithAnIdentityIsRefused(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	dir := t.TempDir()
	key := filepath.Join(dir, "id.age")
	if err := os.WriteFile(key, []byte("AGE-SECRET-KEY-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runRoot(t, "verify", "example.test/repo:tag", "--identity", key); err == nil {
		t.Fatal("an age identity on an unencrypted backup must not be ignored")
	}
}

func TestUnencryptedBackupWithEnvPassphraseIsRefused(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	t.Setenv("BACKIMAGE_PASSPHRASE", cliRestorePass)
	if _, _, err := runRoot(t, "verify", "example.test/repo:tag"); err == nil {
		t.Fatal("BACKIMAGE_PASSPHRASE is a credential like any other")
	}
}

func TestAllowUnencryptedIsTheWayOut(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	dir := t.TempDir()
	passFile := filepath.Join(dir, "pass")
	if err := os.WriteFile(passFile, []byte(cliRestorePass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runRoot(t, "verify", "example.test/repo:tag", "--passphrase-file", passFile, "--allow-unencrypted")
	if err != nil || !strings.Contains(out, "backup integro") {
		t.Fatalf("--allow-unencrypted must restore the old behaviour: %q %v", out, err)
	}
}

// The check must not fire on the ordinary case: no credential, no complaint.
func TestUnencryptedBackupWithoutCredentialStillWorks(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	if _, _, err := runRoot(t, "verify", "example.test/repo:tag"); err != nil {
		t.Fatalf("an unencrypted backup read without a credential must work: %v", err)
	}
}

// Nor on the case it is meant to protect: an encrypted backup with the right
// passphrase.
func TestEncryptedBackupWithPassphraseStillWorks(t *testing.T) {
	s, _ := newMockImageSource(t, true)
	withMockSource(t, s)
	dir := t.TempDir()
	passFile := filepath.Join(dir, "pass")
	if err := os.WriteFile(passFile, []byte(cliRestorePass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runRoot(t, "verify", "example.test/repo:tag", "--passphrase-file", passFile); err != nil {
		t.Fatalf("an encrypted backup with its passphrase must work: %v", err)
	}
}
