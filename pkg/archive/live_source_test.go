package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeChanged archives one regular file whose size changes between the
// lstat that sized its header and the read of its content, then one more
// entry, and returns the tar and the writer's stats. change runs on the open
// file after the lstat.
func writeChanged(t *testing.T, strict bool, change func(path string)) ([]byte, Stats, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	change(path)

	var buf bytes.Buffer
	w := newWriter(&buf, Options{Strict: strict})
	e := &Entry{Path: "root/log", Type: TypeRegular, Size: st.Size(), Mode: 0o644, ModTime: st.ModTime()}
	if err := w.writeEntry(context.Background(), e, path, st, f, nil); err != nil {
		return nil, w.stats, err
	}
	next := &Entry{Path: "root/next", Type: TypeDir, Mode: 0o755, ModTime: st.ModTime()}
	if err := w.writeEntry(context.Background(), next, dir, st, nil, nil); err != nil {
		return nil, w.stats, err
	}
	stats, err := w.Close()
	return buf.Bytes(), stats, err
}

func firstPayload(t *testing.T, archive []byte) (*tar.Header, []byte, int) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	n := 1
	for {
		if _, err := tr.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("tar after the changed entry is corrupt: %v", err)
		}
		n++
	}
	return hdr, data, n
}

func appendTo(path string, n int) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err := f.Write(bytes.Repeat([]byte("b"), n)); err != nil {
		panic(err)
	}
}

// TestDegradedBackupSurvivesAGrowingFile is the regression for a log written
// while it is archived: the copy wrote past the size in the header, tar
// answered "write too long", and the whole backup failed in the one mode
// meant to tolerate a live source.
func TestDegradedBackupSurvivesAGrowingFile(t *testing.T) {
	archive, stats, err := writeChanged(t, false, func(p string) { appendTo(p, 50) })
	if err != nil {
		t.Fatalf("degraded backup failed on a growing file: %v", err)
	}
	hdr, data, n := firstPayload(t, archive)
	if hdr.Size != 100 || len(data) != 100 || strings.Contains(string(data), "b") {
		t.Fatalf("entry holds %d bytes (header %d), want the 100 the header promised", len(data), hdr.Size)
	}
	if n != 2 {
		t.Fatalf("archive holds %d entries, want 2", n)
	}
	if !hasSizeChange(stats) {
		t.Fatalf("the change was not reported: %v", stats.Errors)
	}
}

// TestDegradedBackupSurvivesAShrinkingFile: a file truncated while it is read
// left the tar short of its promised payload, and the next header failed with
// "missed writing".
func TestDegradedBackupSurvivesAShrinkingFile(t *testing.T) {
	archive, stats, err := writeChanged(t, false, func(p string) {
		if err := os.Truncate(p, 40); err != nil {
			panic(err)
		}
	})
	if err != nil {
		t.Fatalf("degraded backup failed on a shrinking file: %v", err)
	}
	hdr, data, n := firstPayload(t, archive)
	if hdr.Size != 100 || len(data) != 100 {
		t.Fatalf("entry holds %d bytes (header %d), want 100", len(data), hdr.Size)
	}
	if !bytes.Equal(data[:40], bytes.Repeat([]byte("a"), 40)) || !bytes.Equal(data[40:], make([]byte, 60)) {
		t.Fatal("a short file must be padded with zeros after what was read")
	}
	if n != 2 {
		t.Fatalf("archive holds %d entries, want 2", n)
	}
	if !hasSizeChange(stats) {
		t.Fatalf("the change was not reported: %v", stats.Errors)
	}
}

// TestStrictBackupRefusesAFileThatChangedSize: strict mode keeps failing,
// with an error that names the change instead of a tar internal.
func TestStrictBackupRefusesAFileThatChangedSize(t *testing.T) {
	for name, change := range map[string]func(string){
		"grow":   func(p string) { appendTo(p, 50) },
		"shrink": func(p string) { _ = os.Truncate(p, 40) },
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := writeChanged(t, true, change)
			if err == nil || !strings.Contains(err.Error(), "size changed") {
				t.Fatalf("strict: err = %v, want a size change error", err)
			}
		})
	}
}

func hasSizeChange(st Stats) bool {
	for _, e := range st.Errors {
		if strings.Contains(e.Error(), "size changed") {
			return true
		}
	}
	return false
}
