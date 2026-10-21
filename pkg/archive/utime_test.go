//go:build unix

package archive

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRestoreAppliesMtimeWithoutAtime: an archive without access times — the
// default, PreserveTimes is off — makes the extractor pass UTIME_OMIT for
// atime. The constant is per-platform; the Linux value used everywhere made
// utimensat fail with EINVAL on macOS and the BSDs, so no restored file or
// directory got its mtime back. This test runs in the macOS CI job.
func TestRestoreAppliesMtimeWithoutAtime(t *testing.T) {
	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	buf := tarWith(t,
		tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mt},
		tar.Header{Name: "d/f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3, ModTime: mt},
	)
	dst := t.TempDir()
	stats, err := extractorFor(ExtractOptions{Strict: true}).Extract(context.Background(), buf, dst)
	if err != nil {
		t.Fatalf("strict extract: %v", err)
	}
	if n := stats.Degraded["times"]; n != 0 {
		t.Fatalf("timestamps degraded on %d entries: %v", n, stats.DegradedExamples)
	}
	for _, p := range []string{"d", "d/f"} {
		fi, err := os.Lstat(filepath.Join(dst, p))
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(mt) {
			t.Fatalf("%s: mtime %v, want %v", p, fi.ModTime(), mt)
		}
	}
}
