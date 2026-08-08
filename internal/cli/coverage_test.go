package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestNewPrinterText(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := NewPrinter(&out, &errBuf, Options{})
	if err := p.Result("hello"); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestErrorUnwrap(t *testing.T) {
	base := errors.New("base")
	e := &Error{Kind: KindNetwork, Msg: "outer", Err: base}
	if !errors.Is(e, base) {
		t.Fatal("expected Unwrap chain")
	}
	if e.Error() != "outer: base" {
		t.Fatalf("Error() = %q", e.Error())
	}
	e2 := &Error{Kind: KindGeneric, Msg: "solo"}
	if e2.Error() != "solo" {
		t.Fatalf("Error() = %q", e2.Error())
	}
}

func TestKindBounds(t *testing.T) {
	if KindGeneric != 1 || KindInterrupted != 7 {
		t.Fatalf("kind values changed: %d..%d", KindGeneric, KindInterrupted)
	}
}

func TestLoggerContext(t *testing.T) {
	ctx := context.Background()
	if LoggerFrom(ctx) == nil {
		t.Fatal("LoggerFrom must return a logger for plain ctx")
	}
	l := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx2 := WithLogger(ctx, l)
	if LoggerFrom(ctx2) != l {
		t.Fatal("LoggerFrom must return the stored logger")
	}
}

func TestNewLoggerForLevels(t *testing.T) {
	var b bytes.Buffer
	l0 := NewLoggerFor(&b, 0)
	l0.Debug("hidden")
	if b.Len() != 0 {
		t.Fatalf("level 0 must not emit debug: %q", b.String())
	}
	l1 := NewLoggerFor(&b, 1)
	l1.Debug("visible")
	if !strings.Contains(b.String(), "visible") {
		t.Fatalf("level 1 must emit debug: %q", b.String())
	}
}

func TestCountVerbose(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{nil, 0},
		{[]string{"backup"}, 0},
		{[]string{"-v"}, 1},
		{[]string{"--verbose", "-v"}, 2},
		{[]string{"--verbose=1"}, 1},
		{[]string{"-v", "-v", "-v"}, 2},
	}
	for _, tc := range cases {
		if got := countVerbose(tc.args); got != tc.want {
			t.Errorf("countVerbose(%v) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

func TestReportErrorTo(t *testing.T) {
	var b bytes.Buffer
	ReportErrorTo(&b, New(KindPermission, "run: sudo backimage", "cannot read %s", "x"))
	if !strings.Contains(b.String(), "error: cannot read x") ||
		!strings.Contains(b.String(), "hint:  run: sudo backimage") {
		t.Fatalf("stderr = %q", b.String())
	}
}

func TestClassify(t *testing.T) {
	if classify(nil) != nil {
		t.Fatal("classify(nil) must be nil")
	}
	var e *Error
	if !errors.As(classify(New(KindGeneric, "", "x")), &e) || e.Kind != KindGeneric {
		t.Fatal("marker errors pass through")
	}
	if !errors.As(classify(errors.New(`unknown command "x" for "backimage"`)), &e) || e.Kind != KindUsage {
		t.Fatal("unknown command must map to usage")
	}
	if !errors.As(classify(errors.New("unknown flag --zzz")), &e) || e.Kind != KindUsage {
		t.Fatal("unknown flag must map to usage")
	}
}

func TestExecuteFlagsLogger(t *testing.T) {
	// Execute must accept global flags before a subcommand.
	err := Execute(context.Background(), []string{"-v", "--json", "version"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
