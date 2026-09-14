//go:build unix

package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// degradedTree builds a root with one readable and one unreadable regular
// file. Skipped as root, which reads a 0000 file regardless of its mode.
func degradedTree(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless of its mode")
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "leggibile.txt"), []byte("dati veri"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(tree, "segreto.txt")
	if err := os.WriteFile(locked, []byte("mai letto"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	return tree
}

// TestAllowDegradedCompletesWithAnUnreadableFile is the end-to-end regression
// test for the defect this whole flag existed to handle and could not.
//
// An unreadable regular file left its entry without a SHA256, the index schema
// refuses a regular entry without one, and the run died in finalize() with
// "invalid backup metadata: entry[N] bad sha256" — after the archive had been
// built, compressed, encrypted and chunked. --allow-degraded therefore failed
// on exactly the input it exists for.
func TestAllowDegradedCompletesWithAnUnreadableFile(t *testing.T) {
	tree := degradedTree(t)
	var progress []string
	res, err := Run(context.Background(), Config{
		RootPaths:     []string{tree},
		Ref:           "example.com/t/degraded:tag1",
		Compression:   "zstd",
		Jobs:          1,
		MaxLayerSize:  1 << 20,
		AllowDegraded: true,
		SelfExtract:   stubSelf,
		Encrypt:       false,
		Runnable:      false,
		Platforms:     []string{"linux/amd64"},
		Output:        "oci-layout",
		OutputPath:    filepath.Join(t.TempDir(), "layout"),
		CheckpointDir: t.TempDir(),
		Resume:        true,
		Progress:      func(msg string) { progress = append(progress, msg) },
	})
	if err != nil {
		t.Fatalf("--allow-degraded must complete with an unreadable file, got: %v", err)
	}
	if res.ContentSkipped != 1 {
		t.Errorf("ContentSkipped = %d, want 1", res.ContentSkipped)
	}
	if res.Files != 2 {
		t.Errorf("Files = %d, want 2: the unreadable file is archived, not dropped", res.Files)
	}
	// The run must say it out loud: a backup holding an empty file where data
	// should be is the one thing --allow-degraded must never hide.
	if !containsSubstring(progress, "SENZA contenuto") {
		t.Errorf("no warning about the dropped content in the run output: %v", progress)
	}
}

// TestStrictRefusesAnUnreadableFile is the other half: the default run does
// not produce a backup with a hole in it. The preflight stops it before any
// work, and the message names the remedy.
func TestStrictRefusesAnUnreadableFile(t *testing.T) {
	tree := degradedTree(t)
	_, err := Run(context.Background(), Config{
		RootPaths:     []string{tree},
		Ref:           "example.com/t/strict:tag1",
		Compression:   "zstd",
		Jobs:          1,
		MaxLayerSize:  1 << 20,
		AllowDegraded: false,
		SelfExtract:   stubSelf,
		Encrypt:       false,
		Runnable:      false,
		Platforms:     []string{"linux/amd64"},
		Output:        "oci-layout",
		OutputPath:    filepath.Join(t.TempDir(), "layout"),
		CheckpointDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a strict run must refuse a tree it cannot read whole")
	}
	if !strings.Contains(err.Error(), "read-all-files") {
		t.Errorf("the refusal must name the missing capability: %v", err)
	}
}

// TestReadableTreeReportsNoContentSkipped is the control: the counter is zero
// whenever nothing was dropped, so a non-zero value always means something.
func TestPipelineReadableTreeReportsNoContentSkipped(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("tutto leggibile"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Config{
		RootPaths:    []string{tree},
		Ref:          "example.com/t/clean:tag1",
		Compression:  "zstd",
		Jobs:         1,
		MaxLayerSize: 1 << 20,
		// Degraded only to skip the privilege preflight, which a non-root test
		// run cannot satisfy; the tree itself is entirely readable, which is
		// the point of the control.
		AllowDegraded: true,
		SelfExtract:   stubSelf,
		Encrypt:       false,
		Runnable:      false,
		Platforms:     []string{"linux/amd64"},
		Output:        "oci-layout",
		OutputPath:    filepath.Join(t.TempDir(), "layout"),
		CheckpointDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ContentSkipped != 0 {
		t.Errorf("ContentSkipped = %d, want 0", res.ContentSkipped)
	}
}

func containsSubstring(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}
