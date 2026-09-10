package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The extractor is a second entry point into the same backup, so the policy
// that refuses an unencrypted backup to a caller holding a credential has to
// hold here too. Enforcing it on the host binary alone would leave the
// substitution attack working through `docker run IMAGE extract`, which is the
// path most restores actually take.

func TestSelfExtractRefusesUnencryptedBackupWithACredential(t *testing.T) {
	f := newCommandFixture(t, false)
	t.Setenv("BACKIMAGE_PASSPHRASE", commandTestPass)

	for _, args := range [][]string{
		{"list", "--root", f.root},
		{"verify", "--root", f.root},
		{"tar", "--root", f.root},
	} {
		t.Run(args[0], func(t *testing.T) {
			_, _, err := captureRun(t, args...)
			if err == nil {
				t.Fatal("a credential on an unencrypted backup must not be ignored")
			}
			if exitCode(err) != exitIntegrity {
				t.Fatalf("want exit %d (integrity), got %d: %v", exitIntegrity, exitCode(err), err)
			}
			if !strings.Contains(err.Error(), "--allow-unencrypted") {
				t.Fatalf("the error must name the way out, got %q", err)
			}
		})
	}

	t.Run("extract", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "dest")
		_, _, err := captureRun(t, "extract", "--root", f.root, "--out", out)
		if err == nil || exitCode(err) != exitIntegrity {
			t.Fatalf("extract must refuse with exit %d, got %d: %v", exitIntegrity, exitCode(err), err)
		}
	})
}

func TestSelfExtractAllowUnencryptedIsTheWayOut(t *testing.T) {
	f := newCommandFixture(t, false)
	t.Setenv("BACKIMAGE_PASSPHRASE", commandTestPass)
	out, _, err := captureRun(t, "list", "--root", f.root, "--allow-unencrypted")
	if err != nil || !strings.Contains(out, "root/a.txt") {
		t.Fatalf("--allow-unencrypted must restore the old behaviour: %q %v", out, err)
	}
}

func TestSelfExtractUnencryptedWithoutCredentialStillWorks(t *testing.T) {
	f := newCommandFixture(t, false)
	out, _, err := captureRun(t, "list", "--root", f.root)
	if err != nil || !strings.Contains(out, "root/a.txt") {
		t.Fatalf("an unencrypted backup read without a credential must work: %q %v", out, err)
	}
}
