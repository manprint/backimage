//go:build unix

package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// SIGTERM used to kill backimage outright: exit 143, and `restore -o` left
// the tar it had started. It has to cancel the run, which then winds down the
// way it does on any other error and exits 7.
//
// The registry accepts the connection and never answers, so the restore is
// parked on a network call that watches its context when the signal lands.
func TestSIGTERMEndsARunWithExitSeven(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	connected := make(chan struct{}, 1)
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	}()

	home := t.TempDir()
	out := filepath.Join(t.TempDir(), "out.tar")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"DOCKER_CONFIG="+filepath.Join(home, "docker"),
		"BACKIMAGE_AUTH_FILE="+filepath.Join(home, "auth.json"),
		helperArgsEnv+"="+strings.Join([]string{"restore", ln.Addr().String() + "/demo:v1", "--output", out}, "\x1f"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-connected:
	case werr := <-exited:
		t.Fatalf("restore ended before reaching the registry: %v\n%s", werr, stderr.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("restore never reached the registry")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var werr error
	select {
	case werr = <-exited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("restore did not end after SIGTERM")
	}
	var exitErr *exec.ExitError
	if !errors.As(werr, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("restore after SIGTERM = %v, want exit status 7\n%s", werr, stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("interrupted restore left %s behind: %v", out, err)
	}
}

// A step that does not watch the context held the process forever after
// SIGTERM: opening a passphrase FIFO that has no writer blocks in the kernel,
// and backup's own signal handler swallowed every further signal too. A
// supervisor's single SIGTERM — `timeout`, systemd, docker stop — has to end
// the run with exit 7 once the grace period is over.
func TestSIGTERMEndsARunBlockedOutsideItsContext(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "pass.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"BACKIMAGE_AUTH_FILE="+filepath.Join(home, "auth.json"),
		helperArgsEnv+"="+strings.Join([]string{"backup", tree, "--repo", "example.test/team/p", "--tag", "t1",
			"--output", "oci-layout", "--output-path", filepath.Join(t.TempDir(), "layout"),
			"--allow-degraded", "--platform", "linux/amd64", "--passphrase-file", fifo}, "\x1f"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// Opening the write end would release the reader, so the test cannot
	// probe that the open is under way; it waits long enough for it instead.
	select {
	case werr := <-exited:
		t.Fatalf("backup ended before blocking on the passphrase FIFO: %v\n%s", werr, stderr.String())
	case <-time.After(2 * time.Second):
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var werr error
	select {
	case werr = <-exited:
	case <-time.After(signalGrace + 10*time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("backup blocked on the passphrase FIFO did not end after SIGTERM\n%s", stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(werr, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("backup after SIGTERM = %v, want exit status 7\n%s", werr, stderr.String())
	}
	if !strings.Contains(stderr.String(), "interrupted") {
		t.Fatalf("stderr does not say the run was interrupted: %q", stderr.String())
	}
}
