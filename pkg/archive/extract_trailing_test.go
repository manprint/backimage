package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// TestExtractReadsWhatFollowsTheEndOfTheArchive is the regression for the
// restore that hung: Extract returned at the tar end marker and left the rest
// of its reader unread, so a producer writing into a pipe — the restore path,
// the self-extracting image — blocked forever on the bytes after it, and the
// restore waited on the producer.
func TestExtractReadsWhatFollowsTheEndOfTheArchive(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		_, err := pw.Write(archive.Bytes())
		if err == nil {
			_, err = pw.Write(bytes.Repeat([]byte("x"), 1<<20))
		}
		_ = pw.CloseWithError(err)
		produced <- err
	}()
	stats, err := extractorFor(ExtractOptions{}).Extract(context.Background(), pr, t.TempDir())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	select {
	case err := <-produced:
		if err != nil {
			t.Fatalf("producer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the producer is still blocked on the bytes after the end of the archive")
	}
	if len(stats.Warnings) == 0 {
		t.Fatal("bytes after the end of the archive were ignored without a word")
	}
}
