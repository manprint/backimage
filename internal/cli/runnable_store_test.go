package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// "none" (and its spelling "store") is the codec docs/compression.md
// recommends for data that is already compressed, and a runnable image
// carries it as a plain tar layer. The CLI rewrote "none" to "store" and then
// refused "store" under the default --runnable=true.
func TestRunnableBackupAcceptsTheStoreCodec(t *testing.T) {
	tree := t.TempDir()
	payload := []byte("already compressed, store it as is")
	if err := os.WriteFile(filepath.Join(tree, "file.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, codec := range []string{"none", "store"} {
		t.Run(codec, func(t *testing.T) {
			layout := filepath.Join(t.TempDir(), "layout")
			if _, stderr, err := runRoot(t, "backup", tree, "--repo", "example.test/team/store", "--tag", "t1",
				"--output", "oci-layout", "--output-path", layout, "--no-encrypt", "--allow-degraded",
				"--platform", "linux/amd64", "--compression", codec); err != nil {
				t.Fatalf("backup --compression %s: %v\n%s", codec, err, stderr)
			}
			dst := t.TempDir()
			if _, stderr, err := runRoot(t, "restore", "example.test/team/store:t1", "--oci-layout", layout,
				"--extract", "--destination", dst, "--no-preserve-owner"); err != nil {
				t.Fatalf("restore: %v\n%s", err, stderr)
			}
			got, err := os.ReadFile(filepath.Join(dst, filepath.Base(tree), "file.bin"))
			if err != nil {
				matches, _ := filepath.Glob(filepath.Join(dst, "*", "file.bin"))
				if len(matches) != 1 {
					t.Fatalf("restored file not found: %v (%v)", err, matches)
				}
				got, err = os.ReadFile(matches[0])
			}
			if err != nil || string(got) != string(payload) {
				t.Fatalf("restored = %q, %v", got, err)
			}
		})
	}
	// The codecs a container runtime cannot unpack stay refused.
	for _, codec := range []string{"xz", "lz4"} {
		_, _, err := runRoot(t, "backup", tree, "--repo", "example.test/team/store", "--tag", "t1",
			"--output", "oci-layout", "--output-path", filepath.Join(t.TempDir(), "layout"), "--no-encrypt",
			"--allow-degraded", "--platform", "linux/amd64", "--compression", codec)
		if ExitCodeFor(err) != int(KindUsage) {
			t.Fatalf("runnable --compression %s = exit %d (%v), want %d", codec, ExitCodeFor(err), err, KindUsage)
		}
	}
}
