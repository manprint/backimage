package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
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

func TestFlagHelpersAndPrinterErrors(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().String("s", "value", "")
	cmd.Flags().Int("i", 3, "")
	cmd.Flags().Bool("b", true, "")
	cmd.Flags().StringSlice("ss", []string{"a"}, "")
	if getFlagString(cmd, "s") != "value" || getFlagInt(cmd, "i") != 3 || !getFlagBool(cmd, "b") || len(getFlagStrings(cmd, "ss")) != 1 {
		t.Fatal("flag helper values")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("missing flag did not panic")
			}
		}()
		_ = getFlagString(cmd, "missing")
	}()
	if err := printerResult(NewPrinter(&failingCLIWriter{}, io.Discard, Options{}), "x"); ExitCodeFor(err) != int(KindGeneric) {
		t.Fatalf("printer failure = %v", err)
	}
	if _, err := parseOptions(&cobra.Command{}); err == nil {
		t.Fatal("parseOptions accepted a root without persistent flags")
	}
}

type failingCLIWriter struct{}

func (*failingCLIWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestNetworkErrorsAndAuthPath(t *testing.T) {
	for _, err := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, errors.New("connection refused by peer"), errors.New("bad SCHEME/HOST")} {
		if !isNetworkErr(err) {
			t.Errorf("not network: %v", err)
		}
	}
	if isNetworkErr(errors.New("other")) {
		t.Fatal("generic error classified as network")
	}
	t.Setenv("BACKIMAGE_AUTH_FILE", "/tmp/custom-auth.json")
	if authFilePath() != "/tmp/custom-auth.json" {
		t.Fatalf("auth path = %q", authFilePath())
	}
	t.Setenv("BACKIMAGE_AUTH_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/config-home")
	if authFilePath() != "/tmp/config-home/backimage/auth.json" {
		t.Fatalf("xdg auth path = %q", authFilePath())
	}
}

func TestAuthHomePromptAndDirectLogoutBranches(t *testing.T) {
	t.Setenv("BACKIMAGE_AUTH_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", t.TempDir())
	if !strings.HasSuffix(authFilePath(), "/.config/backimage/auth.json") {
		t.Fatalf("home auth path = %q", authFilePath())
	}

	oldIn, oldErr := os.Stdin, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin, os.Stderr = r, errFile
	_ = promptOnTTY("password")
	os.Stdin, os.Stderr = oldIn, oldErr
	r.Close()
	errFile.Close()

	root := NewRootCommand()
	logout := newLogoutCommand()
	root.AddCommand(logout)
	if err := runLogout(logout, []string{"a", "b"}); ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("direct logout args = %v", err)
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
