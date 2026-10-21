//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// SIGTERM used to kill the extractor, so it exited 143 on a host and, as PID
// 1 of a container where the kernel ignores it, not at all until docker
// stop's SIGKILL. It has to end the run with the documented exit 7.
//
// The blob is a FIFO: the extractor opening it is the proof that the run is
// under way, and holding it open keeps the run there while the signal lands.
func TestSIGTERMEndsARunWithExitSeven(t *testing.T) {
	f := newCommandFixture(t, false)
	if err := os.Remove(f.blob); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(f.blob, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	out, err := os.Create(filepath.Join(t.TempDir(), "out.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(withoutPassphrase(os.Environ()), helperArgsEnv+"="+strings.Join([]string{"tar", "--root", f.root}, "\x1f"))
	cmd.Stdout = out
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// O_NONBLOCK: opening the write end fails with ENXIO until the reader
	// has opened the other one.
	var fifo *os.File
	deadline := time.Now().Add(30 * time.Second)
	for fifo == nil {
		fifo, err = os.OpenFile(f.blob, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		select {
		case werr := <-exited:
			t.Fatalf("extractor ended before reading the blob: %v\n%s", werr, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("extractor never opened the blob")
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The read of the FIFO does not watch the context: give the signal time
	// to cancel it, then let the read end.
	select {
	case werr := <-exited:
		fifo.Close()
		checkInterrupted(t, werr, stderr.String())
		return
	case <-time.After(time.Second):
	}
	fifo.Close()
	select {
	case werr := <-exited:
		checkInterrupted(t, werr, stderr.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("extractor did not end after SIGTERM")
	}
}

func checkInterrupted(t *testing.T, err error, stderr string) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("extractor exit = %v, want status %d\n%s", err, exitInterrupted, stderr)
	}
	if code := exitErr.ExitCode(); code != exitInterrupted {
		t.Fatalf("extractor exit status = %d (%v), want %d\n%s", code, exitErr, exitInterrupted, stderr)
	}
	if !strings.Contains(stderr, "interrotto") {
		t.Fatalf("stderr does not say the run was interrupted: %q", stderr)
	}
}

func withoutPassphrase(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "BACKIMAGE_PASSPHRASE=") {
			out = append(out, kv)
		}
	}
	return out
}

// The read of a blob nobody writes does not watch the context, so SIGTERM
// alone never ended it: the extractor waited for a second signal, and as PID 1
// of a container it waited for docker stop's SIGKILL. The grace period after
// the first signal has to end it with exit 7.
func TestSIGTERMEndsARunBlockedOutsideItsContext(t *testing.T) {
	f := newCommandFixture(t, false)
	if err := os.Remove(f.blob); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(f.blob, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(withoutPassphrase(os.Environ()), helperArgsEnv+"="+strings.Join([]string{"tar", "--root", f.root}, "\x1f"))
	cmd.Stdout = io.Discard
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// The write end is never opened: the extractor stays in the open.
	select {
	case werr := <-exited:
		t.Fatalf("extractor ended before blocking on the blob: %v\n%s", werr, stderr.String())
	case <-time.After(2 * time.Second):
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case werr := <-exited:
		checkInterrupted(t, werr, stderr.String())
	case <-time.After(signalGrace + 10*time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("extractor blocked on the blob did not end after SIGTERM\n%s", stderr.String())
	}
}
