package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backupToLayout writes an encrypted backup into an OCI layout and returns
// the layout directory, the reference and the digest the backup reported.
func backupToLayout(t *testing.T, passphrase string) (layout, ref, digest string) {
	t.Helper()
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "file.txt"), []byte("anchored payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout = filepath.Join(t.TempDir(), "layout")
	ref = "example.test/team/anchor:t1"
	out, _, err := runRoot(t, "backup", tree, "--repo", "example.test/team/anchor", "--tag", "t1",
		"--output", "oci-layout", "--output-path", layout, "--password", passphrase,
		"--allow-degraded", "--platform", "linux/amd64", "--json")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	var res struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("backup JSON %q: %v", out, err)
	}
	if !strings.HasPrefix(res.Digest, "sha256:") {
		t.Fatalf("backup reported no digest: %q", out)
	}
	return layout, ref, res.Digest
}

// The anchor has to be checked before the secret is handed over, and the
// evidence for that cannot be "the command failed": it must be that the
// passphrase was never read. The control run below proves the CLI does read
// that file at this point when nothing refuses the image first.
func TestExpectDigestRefusesBeforeReadingThePassphrase(t *testing.T) {
	layoutDir, ref, digest := backupToLayout(t, "anchor-password")
	missingSecret := filepath.Join(t.TempDir(), "not-on-disk")
	dst := t.TempDir()

	// Control: without an anchor the passphrase file is read, and its
	// absence is what the command complains about.
	_, _, err := runRoot(t, "restore", ref, "--oci-layout", layoutDir, "-x", "-C", dst,
		"--passphrase-file", missingSecret)
	if err == nil {
		t.Fatal("a missing passphrase file must fail the restore")
	}
	if !strings.Contains(err.Error(), "not-on-disk") {
		t.Fatalf("the control run must fail on the passphrase file, got %v", err)
	}

	// With a digest that does not match, the same command must stop earlier:
	// integrity class, and no mention of the passphrase file it never opened.
	wrong := "sha256:" + strings.Repeat("33", 32)
	_, _, err = runRoot(t, "restore", ref, "--oci-layout", layoutDir, "-x", "-C", dst,
		"--passphrase-file", missingSecret, "--expect-digest", wrong)
	if err == nil {
		t.Fatal("a mismatched --expect-digest must fail the restore")
	}
	if got := ExitCodeFor(err); got != int(KindIntegrity) {
		t.Fatalf("exit code = %d, want %d (integrity)", got, KindIntegrity)
	}
	if !strings.Contains(err.Error(), wrong) || !strings.Contains(err.Error(), digest) {
		t.Fatalf("the refusal must name both digests: %v", err)
	}
	if strings.Contains(err.Error(), "not-on-disk") {
		t.Fatalf("the passphrase file was read before the anchor was checked: %v", err)
	}
}

func TestExpectDigestAcceptsTheDigestTheBackupReported(t *testing.T) {
	layoutDir, ref, digest := backupToLayout(t, "anchor-password")
	secret := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(secret, []byte("anchor-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, _, err := runRoot(t, "restore", ref, "--oci-layout", layoutDir, "-x", "-C", dst,
		"--passphrase-file", secret, "--expect-digest", digest); err != nil {
		t.Fatalf("the reported digest must be accepted: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, filepath.Base(dst)))
	if err == nil && len(got) == 0 {
		t.Fatal("empty restore")
	}
}

func TestExpectDigestRejectsAMalformedValue(t *testing.T) {
	layoutDir, ref, _ := backupToLayout(t, "anchor-password")
	_, _, err := runRoot(t, "verify", ref, "--oci-layout", layoutDir, "--expect-digest", "deadbeef")
	if err == nil {
		t.Fatal("a malformed digest must be a usage error")
	}
	if got := ExitCodeFor(err); got != int(KindUsage) {
		t.Fatalf("exit code = %d, want %d (usage)", got, KindUsage)
	}
}
