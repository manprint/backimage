package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExitCodeTable(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{New(KindGeneric, "", "boom"), 1},
		{New(KindUsage, "", "boom"), 2},
		{New(KindPermission, "", "boom"), 3},
		{New(KindPassphrase, "", "boom"), 4},
		{New(KindIntegrity, "", "boom"), 5},
		{New(KindNetwork, "", "boom"), 6},
		{New(KindInterrupted, "", "boom"), 7},
		{fmt.Errorf("wrapped: %w", New(KindUsage, "", "boom")), 2},
		{fmt.Errorf("wrapped: %w", ErrPassphrase), 4},
		{errors.New("plain"), 1},
	}
	for _, tc := range cases {
		if got := ExitCodeFor(tc.err); got != tc.want {
			t.Errorf("ExitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestErrorWithHintFormatting(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := NewPrinter(&out, &errBuf, Options{})
	p.Warnf("keep this")
	ReportErrorTo(&errBuf, New(KindUsage, "use --flag instead", "bad argument %q", "x"))
	if out.Len() != 0 {
		t.Fatalf("stdout must be empty, got %q", out.String())
	}
	lines := strings.Split(strings.TrimSpace(errBuf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines on stderr, got %d: %q", len(lines), errBuf.String())
	}
	if !strings.HasPrefix(lines[1], "error: bad argument") {
		t.Errorf("line 2 = %q, want error line", lines[1])
	}
	if !strings.HasPrefix(lines[2], "hint:  use --flag") {
		t.Errorf("line 3 = %q, want hint line", lines[2])
	}
}

func TestJSONPrinterSingleObject(t *testing.T) {
	var out bytes.Buffer
	p := NewPrinter(&out, &bytes.Buffer{}, Options{JSON: true})
	if err := p.Result(map[string]any{"a": 1}); err != nil {
		t.Fatalf("Result: %v", err)
	}
	s := strings.TrimSpace(out.String())
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		t.Fatalf("expected single JSON object, got %q", s)
	}
	if strings.Count(s, "{") != 1 {
		t.Fatalf("expected exactly one object, got %q", s)
	}
}

func TestInfofNeverWritesStdout(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := NewPrinter(&out, &errBuf, Options{})
	p.Infof("progress %d", 1)
	p.Warnf("careful")
	p.Infof("more")
	if out.Len() != 0 {
		t.Fatalf("Infof/Warnf leaked %d bytes to stdout: %q", out.Len(), out.String())
	}
	if !strings.Contains(errBuf.String(), "progress 1") {
		t.Fatalf("progress missing from stderr: %q", errBuf.String())
	}
}

func TestQuietSuppressesInfof(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := NewPrinter(&out, &errBuf, Options{Quiet: true})
	p.Infof("hidden")
	p.Warnf("visible")
	if errBuf.String() != "warning: visible\n" {
		t.Fatalf("stderr = %q", errBuf.String())
	}
}
