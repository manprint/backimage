package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRestoreDrainsBytesAfterTheEndOfTheArchive is the regression for the
// restore that hung: the extractor stopped reading at the tar end marker, the
// producer blocked forever writing whatever came after it into the pipe, and
// the restore waited on the producer. The rest of the stream is now read, so
// the producer finishes — and verifies — the chunks it still holds.
func TestRestoreDrainsBytesAfterTheEndOfTheArchive(t *testing.T) {
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
	trailing := bytes.Repeat([]byte("trailing"), 64<<10)
	stream := func(w io.Writer) error {
		if _, err := w.Write(archive.Bytes()); err != nil {
			return err
		}
		_, err := w.Write(trailing)
		return err
	}

	dest := t.TempDir()
	cmd, _, err := NewRootCommand().Find([]string{"restore"})
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetContext(context.Background())
	cmd.SetErr(io.Discard)
	if err := cmd.ParseFlags([]string{"--destination", dest, "--no-preserve-owner"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := restoreExtract(cmd, stream, false, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the restore hung on bytes after the end of the archive")
	}
	if got, err := os.ReadFile(filepath.Join(dest, "f")); err != nil || string(got) != "ok" {
		t.Fatalf("restored file = %q, %v", got, err)
	}
}
