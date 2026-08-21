package recovery

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// A single damaged chunk used to cost the whole restore. The partial recovery
// must salvage every entry that lives elsewhere, name the ones it lost, and
// still emit a tar a normal reader can parse to the end.
func TestStreamTarPartialSalvagesIntactEntries(t *testing.T) {
	f := makeFixture(t, false, 2048)
	data, err := os.ReadFile(f.chunkPath)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the middle of the blob: the first chunks stay intact.
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(f.chunkPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	idx, err := b.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The plain restore gives up entirely.
	var whole bytes.Buffer
	if err := b.StreamTar(context.Background(), &whole, true); err == nil {
		t.Fatal("the sequential restore must fail on a corrupted chunk")
	}

	var out bytes.Buffer
	report, err := b.StreamTarPartial(context.Background(), idx, &out, true)
	if err != nil {
		t.Fatalf("the partial recovery must not fail: %v", err)
	}
	if report.Entries == 0 {
		t.Fatal("no entry was salvaged")
	}
	if report.Skipped == 0 {
		t.Fatal("the corrupted chunk must cost at least one entry")
	}
	if len(report.BadChunks) == 0 {
		t.Fatal("the damaged chunk must be named")
	}
	if report.Entries+report.Skipped != len(idx.Entries) {
		t.Fatalf("entries = %d, skipped = %d, index has %d", report.Entries, report.Skipped, len(idx.Entries))
	}
	if len(report.SkippedPaths) == 0 {
		t.Fatal("the lost paths are the point of the report")
	}

	// What was written must be a readable tar, and only contain salvaged
	// entries: a truncated record would break every entry after it.
	tr := tar.NewReader(&out)
	names := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the salvaged tar must stay parseable: %v", err)
		}
		names++
	}
	if names != report.Entries {
		t.Fatalf("tar holds %d entries, report claims %d", names, report.Entries)
	}
	summary := strings.Join(report.Summary(), "\n")
	if !strings.Contains(summary, "ATTENZIONE") || !strings.Contains(summary, "percorsi perduti") {
		t.Fatalf("the evidence must be explicit: %q", summary)
	}
}

// With nothing damaged the report says so, and the tar is complete.
func TestStreamTarPartialOnIntactBackup(t *testing.T) {
	f := makeFixture(t, false, 2048)
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	idx, err := b.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report, err := b.StreamTarPartial(context.Background(), idx, &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 0 || len(report.BadChunks) != 0 {
		t.Fatalf("report = %+v, want nothing skipped", report)
	}
	if report.Entries != len(idx.Entries) {
		t.Fatalf("entries = %d, index has %d", report.Entries, len(idx.Entries))
	}
	if s := strings.Join(report.Summary(), "\n"); !strings.Contains(s, "non necessario") {
		t.Fatalf("summary = %q", s)
	}
}

func TestStreamTarPartialNeedsIndex(t *testing.T) {
	f := makeFixture(t, false, 2048)
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.StreamTarPartial(context.Background(), nil, io.Discard, true); err == nil {
		t.Fatal("a nil index must be refused")
	}
}
