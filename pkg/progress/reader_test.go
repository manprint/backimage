package progress

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestReaderReportsConsumedBytes(t *testing.T) {
	var reports []int64
	r := NewReader(bytes.NewReader([]byte("payload")), func(done int64) {
		reports = append(reports, done)
	})
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if len(reports) == 0 || reports[len(reports)-1] != int64(len("payload")) {
		t.Fatalf("reports = %v", reports)
	}
}

func TestMessage(t *testing.T) {
	if got := Message("restore", 5<<20, 10<<20); !strings.Contains(got, "50%") {
		t.Fatalf("message = %q", got)
	}
	if got := Message("restore", 5<<20, 0); !strings.Contains(got, "5.0 MiB elaborati") {
		t.Fatalf("unknown-total message = %q", got)
	}
}
