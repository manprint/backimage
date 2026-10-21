//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupQuietKeepsWarnings: --quiet suppresses progress, not warnings.
// The count of files archived without their content went out as a progress
// line, so an unattended --quiet --allow-degraded run produced a backup with
// empty files and said nothing about it.
func TestBackupQuietKeepsWarnings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless of its mode")
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "readable.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(tree, "locked.txt")
	if err := os.WriteFile(locked, []byte("never read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	layout := filepath.Join(t.TempDir(), "layout")
	_, stderr, err := runRoot(t, "backup", tree, "--repo", "example.test/team/quiet", "--tag", "t1",
		"--output", "oci-layout", "--output-path", layout, "--no-encrypt", "--allow-degraded",
		"--platform", "linux/amd64", "--quiet")
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "warning: backup: 1 file archiviati SENZA contenuto") {
		t.Fatalf("--quiet dropped the warning about the lost content; stderr = %q", stderr)
	}
	if strings.Contains(stderr, "upload") || strings.Contains(stderr, "preparazione") {
		t.Fatalf("--quiet printed progress: %q", stderr)
	}
}
