package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

// helperArgsEnv makes the test binary run main() with the arguments it
// holds, separated by \x1f, so a test can drive the real process: its signal
// handling and its exit status.
const helperArgsEnv = "BACKIMAGE_SELFEXTRACT_TEST_MAIN"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(helperArgsEnv); ok {
		os.Args = append([]string{"backimage-selfextract"}, strings.Split(args, "\x1f")...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// An empty first argument indexed args[0][0] and panicked.
func TestAnEmptyArgumentIsAUsageError(t *testing.T) {
	_, _, err := captureRun(t, "")
	if exitCode(err) != exitUsage {
		t.Fatalf("empty argument exit = %d (%v), want %d", exitCode(err), err, exitUsage)
	}
}

// A run cut short by a signal exits 7 whatever the command made of the
// cancellation: tar used to call every stream error damage, exit 5.
func TestAnInterruptedRunExitsSeven(t *testing.T) {
	f := newCommandFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{
		{"tar", "--root", f.root},
		{"extract", "--root", f.root, "--out", t.TempDir(), "--no-preserve-owner"},
		{"verify", "--root", f.root},
	} {
		oldOut, oldErr, oldTTY := stdout, stderr, stdoutIsTerminal
		var sink strings.Builder
		stdout, stderr = &sink, &sink
		stdoutIsTerminal = func() bool { return false }
		err := interrupted(ctx, run(ctx, args))
		stdout, stderr, stdoutIsTerminal = oldOut, oldErr, oldTTY
		if err == nil {
			// Nothing in the command looked at the context: nothing was
			// interrupted, and 0 is the right answer.
			continue
		}
		if exitCode(err) != exitInterrupted {
			t.Fatalf("%s: exit = %d (%v), want %d", args[0], exitCode(err), err, exitInterrupted)
		}
	}
	if err := interrupted(context.Background(), errors.New("boom")); exitCode(err) != exitGeneric {
		t.Fatalf("a failure without a signal became %d", exitCode(err))
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// A destination that refuses the tar is not a damaged backup: tar mapped the
// ENOSPC of a full disk to exit 5.
func TestTarReportsAFailedOutputAsAnOutputError(t *testing.T) {
	f := newCommandFixture(t, false)
	oldOut, oldErr, oldTTY := stdout, stderr, stdoutIsTerminal
	defer func() { stdout, stderr, stdoutIsTerminal = oldOut, oldErr, oldTTY }()
	var log strings.Builder
	stdout, stderr = failingWriter{err: syscall.ENOSPC}, &log
	stdoutIsTerminal = func() bool { return false }
	err := run(context.Background(), []string{"tar", "--root", f.root})
	if exitCode(err) != exitGeneric || !errors.Is(err, syscall.ENOSPC) || !strings.Contains(err.Error(), "stdout") {
		t.Fatalf("tar to a full disk = exit %d (%v), want %d naming stdout", exitCode(err), err, exitGeneric)
	}
}
