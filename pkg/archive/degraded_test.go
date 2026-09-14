//go:build unix

package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// emptyDigest is what a regular entry with no payload must carry.
func emptyDigest() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}

// unreadableTree builds a root holding one readable and one unreadable regular
// file, and returns the root. Skipped as root, which ignores the mode.
func unreadableTree(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless of its mode")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "leggibile.txt"), []byte("ciao"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(src, "segreto.txt")
	if err := os.WriteFile(locked, []byte("contenuto che non si può leggere"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	return src
}

func entryBySuffix(t *testing.T, entries []Entry, suffix string) Entry {
	t.Helper()
	for _, e := range entries {
		if strings.HasSuffix(e.Path, suffix) {
			return e
		}
	}
	t.Fatalf("entry %q not emitted; got %v", suffix, entryPaths(entries))
	return Entry{}
}

func entryPaths(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

// TestDegradedUnreadableFileCarriesTheDigestOfItsPayload is the regression
// test for the defect that made --allow-degraded useless: the entry of a file
// that could not be opened was emitted with an empty SHA256, and the index
// schema rejects a regular entry without a digest, so the backup died at the
// metadata step with "entry[N] bad sha256" — after archiving, compressing and
// encrypting everything.
//
// The archive really does hold zero bytes for that entry, so the digest it
// carries must be the digest of zero bytes. What says the content is missing
// is Stats.ContentSkipped, not a malformed digest.
func TestDegradedUnreadableFileCarriesTheDigestOfItsPayload(t *testing.T) {
	src := unreadableTree(t)

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: false})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatalf("degraded walk must not fail: %v", err)
	}
	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}

	locked := entryBySuffix(t, w.Entries(), "/segreto.txt")
	if locked.Type != TypeRegular {
		t.Fatalf("segreto.txt type = %v, want regular", locked.Type)
	}
	if locked.Size != 0 {
		t.Errorf("segreto.txt size = %d, want 0: the payload was never read", locked.Size)
	}
	if locked.SHA256 != emptyDigest() {
		t.Errorf("segreto.txt sha256 = %q, want the digest of an empty payload %q",
			locked.SHA256, emptyDigest())
	}
	if len(locked.SHA256) != 64 {
		t.Errorf("sha256 %q is not 64 hex digits: the index schema refuses it", locked.SHA256)
	}
	// The mode survives even though the content did not: the entry is still a
	// faithful description of the file, minus its bytes.
	if locked.Mode.Perm() != 0 {
		t.Errorf("segreto.txt mode = %v, want 0 (metadata is preserved)", locked.Mode.Perm())
	}

	readable := entryBySuffix(t, w.Entries(), "/leggibile.txt")
	if readable.SHA256 == emptyDigest() {
		t.Error("leggibile.txt must carry the digest of its real content")
	}

	if stats.ContentSkipped != 1 {
		t.Errorf("ContentSkipped = %d, want 1", stats.ContentSkipped)
	}
	if len(stats.Errors) == 0 {
		t.Error("the open failure must stay in Stats.Errors")
	}
	if !hasWarning(stats.Warnings, "archiviati come file vuoti") {
		t.Errorf("a run that drops content must warn once; warnings = %v", stats.Warnings)
	}
}

// TestDegradedDigestsAreAllWellFormed states the invariant the index schema
// enforces, over every entry of a degraded run: no regular entry may leave the
// writer without 64 hex digits of digest.
func TestDegradedDigestsAreAllWellFormed(t *testing.T) {
	src := unreadableTree(t)
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: false})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, e := range w.Entries() {
		if e.Type != TypeRegular {
			continue
		}
		if len(e.SHA256) != 64 {
			t.Errorf("entry %q: sha256 %q is not 64 hex digits", e.Path, e.SHA256)
			continue
		}
		if _, err := hex.DecodeString(e.SHA256); err != nil {
			t.Errorf("entry %q: sha256 %q is not hex: %v", e.Path, e.SHA256, err)
		}
	}
}

// TestDegradedArchiveStaysAReadableTar proves the tar is not merely
// well-described but actually parseable: an entry announcing a size it does
// not carry would make the next header unreadable.
func TestDegradedArchiveStaysAReadableTar(t *testing.T) {
	src := unreadableTree(t)
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: false})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	x := NewExtractor(ExtractOptions{Strict: false})
	if _, err := x.Extract(context.Background(), &buf, dst); err != nil {
		t.Fatalf("a degraded archive must still extract: %v", err)
	}
	restored := filepath.Join(dst, filepath.Base(src), "segreto.txt")
	fi, err := os.Stat(restored)
	if err != nil {
		t.Fatalf("the unreadable file must still be restored, as an empty file: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("restored size = %d, want 0", fi.Size())
	}
}

// TestStrictUnreadableFileAborts is the other half of the contract: without
// --allow-degraded, a file whose content cannot be read stops the backup
// instead of silently producing an empty one.
func TestStrictUnreadableFileAborts(t *testing.T) {
	src := unreadableTree(t)
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: true})
	err := w.AddRoot(context.Background(), src)
	if err == nil {
		t.Fatal("strict mode must refuse an unreadable file")
	}
	if !strings.Contains(err.Error(), "segreto.txt") {
		t.Errorf("the error must name the file it refused: %v", err)
	}
}

// TestReadableTreeReportsNoContentSkipped is the control: the counter stays at
// zero when nothing was dropped, so a non-zero value always means something.
func TestReadableTreeReportsNoContentSkipped(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: true})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.ContentSkipped != 0 {
		t.Errorf("ContentSkipped = %d, want 0", stats.ContentSkipped)
	}
	if len(stats.Warnings) != 0 {
		t.Errorf("a clean run must warn about nothing: %v", stats.Warnings)
	}
}

// TestToleratedXattrLossIsStillCountedAsADifference is the contract the
// --strict exit code of the CLI reads.
//
// An attribute the destination refuses outright, and trusted.* without
// CAP_SYS_ADMIN, are tolerated even in strict mode: stopping halfway would
// leave a partial tree behind a loss nothing could have prevented there. That
// is only defensible because the loss is still counted — Stats.Degraded is
// what the caller turns into a non-zero exit, and FidelityLines is what the
// user reads. A tolerated loss that left both silent would be the restore
// claiming a fidelity it did not deliver.
func TestToleratedXattrLossIsStillCountedAsADifference(t *testing.T) {
	if runtime.GOOS != "linux" {
		// The two tolerated losses are Linux rules: namespaces, and trusted.*
		// behind CAP_SYS_ADMIN. Darwin has neither, so it accepts both names
		// and there is no loss here to count.
		t.Skip("xattr namespaces and CAP_SYS_ADMIN are Linux rules")
	}
	if os.Geteuid() == 0 {
		t.Skip("root holds CAP_SYS_ADMIN: trusted.* is writable")
	}
	for _, tc := range []struct {
		name  string
		attr  string
		class string
	}{
		{"trusted without CAP_SYS_ADMIN", "trusted.overlay.opaque", "xattr.trusted"},
		{"namespace the filesystem refuses", "bogusns.attr", "xattr.bogusns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			x := NewExtractor(ExtractOptions{PreserveXattrs: true, Strict: true})
			stats, err := x.Extract(context.Background(), tarWithXattr(t, tc.attr, "v"), dst)
			if err != nil {
				t.Fatalf("a tolerated loss must not abort the extraction: %v", err)
			}
			if stats.Degraded[tc.class] != 1 {
				t.Fatalf("Degraded = %v, want one %q: the caller cannot report what it is not told",
					stats.Degraded, tc.class)
			}
			if stats.DegradedExamples[tc.class] == "" {
				t.Errorf("class %q has no example failure to show the user", tc.class)
			}
			lines := stats.FidelityLines()
			if len(lines) == 0 || !strings.Contains(lines[0], "NON 1:1") {
				t.Fatalf("verdict must state the restore was not 1:1, got %v", lines)
			}
		})
	}
}

// TestFifoXattrIsAnUnopenableDifference covers the one metadata loss a wholly
// unprivileged restore can hit on any filesystem, which is what the e2e phase
// uses to exercise the --strict exit code: a fifo (or symlink, or device) that
// carries extended attributes.
//
// There is no *at form of setxattr, so the object would have to be opened by
// name to receive them, and opening a fifo blocks while opening a device node
// has effects on the device. The attributes are dropped — and counted, with an
// example naming the object, because with --strict this class is what decides
// whether the command succeeds.
func TestFifoXattrIsAnUnopenableDifference(t *testing.T) {
	var buf bytes.Buffer
	tw := newFifoTarWithXattr(t, &buf, "system.posix_acl_access")

	dst := t.TempDir()
	x := NewExtractor(ExtractOptions{PreserveXattrs: true, Strict: true})
	stats, err := x.Extract(context.Background(), tw, dst)
	if err != nil {
		t.Fatalf("an unopenable object must not abort even a strict restore: %v", err)
	}
	if stats.Fifos != 1 {
		t.Fatalf("the fifo itself must still be created: %+v", stats)
	}
	if stats.Degraded["xattr.unopenable"] != 1 {
		t.Fatalf("Degraded = %v, want one xattr.unopenable", stats.Degraded)
	}
	if got := stats.DegradedExamples["xattr.unopenable"]; !strings.Contains(got, "pipe") {
		t.Errorf("the example must name the object it could not write to, got %q", got)
	}
	if lines := stats.FidelityLines(); len(lines) == 0 || !strings.Contains(lines[0], "NON 1:1") {
		t.Errorf("verdict must state the restore was not 1:1, got %v", lines)
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// newFifoTarWithXattr forges a one-entry archive holding a fifo that carries
// an extended attribute. Building it on disk instead would need a valid raw
// ACL blob; what is under test is the extractor's branch, not the encoding.
func newFifoTarWithXattr(t *testing.T, buf *bytes.Buffer, attr string) *bytes.Buffer {
	t.Helper()
	tw := tar.NewWriter(buf)
	hdr := &tar.Header{
		Name:       "pipe",
		Mode:       0o644,
		Typeflag:   tar.TypeFifo,
		Format:     tar.FormatPAX,
		PAXRecords: map[string]string{"SCHILY.xattr." + attr: "\x02\x00\x00\x00"},
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf
}

// TestClosingVerdict covers the line both binaries end an extraction with.
// It is what a person greps in a cron mail and what a script matches on, so
// its two shapes have to be unmistakable and its numbers have to be the ones
// the rest of the report shows.
func TestClosingVerdict(t *testing.T) {
	clean := Stats{Files: 3, Dirs: 2, Symlinks: 1}
	got := clean.ClosingVerdict(true)
	for _, want := range []string{"estrazione 1:1", "nessun errore", "6 oggetti", "0 differenze", "0 entry saltate", "tutti i chunk verificati"} {
		if !strings.Contains(got, want) {
			t.Errorf("clean verdict %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "NON 1:1") {
		t.Errorf("clean verdict must not read as a failure: %q", got)
	}

	// --no-verify has to be visible in the verdict: a restore nobody
	// authenticated is not the same answer as one that was.
	if got := clean.ClosingVerdict(false); !strings.Contains(got, "NON verificati") {
		t.Errorf("unverified verdict %q does not say the digests were skipped", got)
	}

	degraded := Stats{Files: 3, Dirs: 2, Skipped: 1, Degraded: map[string]int64{"owner": 2, "xattr.system": 1}}
	got = degraded.ClosingVerdict(true)
	for _, want := range []string{"NON 1:1", "3 differenze di metadati", "1 entry non create"} {
		if !strings.Contains(got, want) {
			t.Errorf("degraded verdict %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "nessun errore") {
		t.Errorf("a degraded verdict must never claim there were no errors: %q", got)
	}
}

// TestClosingVerdictAgreesWithFidelityLines keeps the one-line verdict and the
// detailed report from ever disagreeing: they are read together, and a summary
// that says 1:1 above a list of differences would be worse than no summary.
func TestClosingVerdictAgreesWithFidelityLines(t *testing.T) {
	for _, s := range []Stats{
		{Files: 1},
		{Files: 1, Skipped: 1},
		{Files: 1, Degraded: map[string]int64{"mode": 1}},
		{Files: 1, Degraded: map[string]int64{}},
	} {
		verdictClean := !strings.Contains(s.ClosingVerdict(true), "NON 1:1")
		linesClean := strings.Contains(s.FidelityLines()[0], "esito 1:1")
		if verdictClean != linesClean {
			t.Errorf("stats %+v: verdict says clean=%v, FidelityLines says clean=%v", s, verdictClean, linesClean)
		}
	}
}
