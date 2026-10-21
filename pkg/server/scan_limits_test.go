package server

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/manprint/backimage/pkg/index"
)

func tarOf(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, h := range headers {
		h.Format = tar.FormatPAX
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestScanArchiveStopsAtTheEntryLimit is the regression for the unbounded
// scan: every entry of the received tar was kept in memory, and the index
// limit was only applied when the index was written, after the upload. A
// client with millions of empty files held the server's memory.
func TestScanArchiveStopsAtTheEntryLimit(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}
	headers := make([]*tar.Header, 0, len(names))
	for _, name := range names {
		headers = append(headers, &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644})
	}
	stream := tarOf(t, headers...)
	counter := &countingReader{r: bytes.NewReader(stream)}
	entries, _, err := scanWithLimit(counter, 3)
	if !errors.Is(err, index.ErrBadSchema) || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("scan of 5 entries with a limit of 3 = %v, want an entry limit refusal", err)
	}
	if len(entries) > 3 {
		t.Fatalf("the scan kept %d entries past the limit", len(entries))
	}
	if counter.n >= int64(len(stream)) {
		t.Fatal("the scan read the whole stream before refusing it")
	}
}

// TestScanArchiveRefusesNamesNoIndexCanHold: a path or a link target longer
// than the index accepts is refused while scanning, not after the upload.
func TestScanArchiveRefusesNamesNoIndexCanHold(t *testing.T) {
	long := strings.Repeat("p", index.MaxPathBytes+1)
	for name, h := range map[string]*tar.Header{
		"path":   {Name: long, Typeflag: tar.TypeReg, Mode: 0o644},
		"target": {Name: "l", Typeflag: tar.TypeSymlink, Linkname: long},
		"owner":  {Name: "f", Typeflag: tar.TypeReg, Mode: 0o644, Uname: long},
	} {
		if _, _, err := scanWithLimit(bytes.NewReader(tarOf(t, h)), 10); !errors.Is(err, index.ErrBadSchema) {
			t.Errorf("%s: scan = %v, want a refusal", name, err)
		}
	}
}

// TestIngestStopsAtABadEntryBeforeTheRestIsReceived: a tar the scanner
// refuses used to be found out only at the end, after every layer had been
// built and pushed. The pipeline now stops on the scanner's error.
func TestIngestStopsAtABadEntryBeforeTheRestIsReceived(t *testing.T) {
	bad := tarOf(t, &tar.Header{Name: "odd", Typeflag: 'Z', Mode: 0o644})
	bad = bad[:512+512] // the PAX record and the header, no end marker
	const total = 256 << 20
	start, _ := streamStartFor(t, total, false)
	sink := newStreamSink()
	in, err := startIngest(context.Background(), ingestConfig{
		Start: start, Reference: start.Reference, TempDir: t.TempDir(), Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	if err := in.Write(bad); err == nil {
		accepted += len(bad)
		filler := make([]byte, 1<<20)
		for accepted < total {
			if err := in.Write(filler); err != nil {
				break
			}
			accepted += len(filler)
		}
	}
	_, err = in.Finish()
	if err == nil || !strings.Contains(err.Error(), "typeflag") {
		t.Fatalf("Finish = %v, want the scanner's refusal", err)
	}
	if accepted >= total {
		t.Fatalf("the whole %d byte stream was received after the scanner refused its first entry", total)
	}
}

var _ io.Reader = (*countingReader)(nil)
