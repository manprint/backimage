package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A pinning client trusts a fingerprint, so the pin must survive a restart.
func TestSelfSignedCertificatePersistsInWorkDir(t *testing.T) {
	workDir := t.TempDir()
	pins := make([]string, 2)
	for i := range pins {
		cmd := newListenRemoteCommand()
		if err := cmd.ParseFlags([]string{"--tls-self-signed"}); err != nil {
			t.Fatal(err)
		}
		_, pin, _, err := listenTLSConfig(cmd, "0.0.0.0:7575", workDir, NewPrinter(io.Discard, io.Discard, Options{}))
		if err != nil {
			t.Fatal(err)
		}
		pins[i] = pin
	}
	if pins[0] != pins[1] {
		t.Fatalf("pin changed across restarts: %q then %q", pins[0], pins[1])
	}
	keyPath := filepath.Join(workDir, selfSignedDirName, "self-signed.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key mode = %o, want 600", perm)
	}
}

func TestSelfSignedCertificatePersistsAtExplicitPaths(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "sub", "server.crt")
	keyPath := filepath.Join(dir, "sub", "server.key")
	run := func() string {
		cmd := newListenRemoteCommand()
		if err := cmd.ParseFlags([]string{"--tls-self-signed", "--tls-cert", certPath, "--tls-key", keyPath}); err != nil {
			t.Fatal(err)
		}
		_, pin, _, err := listenTLSConfig(cmd, "127.0.0.1:7575", "", NewPrinter(io.Discard, io.Discard, Options{}))
		if err != nil {
			t.Fatal(err)
		}
		return pin
	}
	first := run()
	if first != run() {
		t.Fatal("explicit self-signed paths did not reuse the certificate")
	}
	if _, err := os.Stat(certPath); err != nil {
		t.Fatal(err)
	}
}

// Improvement 1: the pin is what the client passes to --tls-pin, so a provided
// certificate must report it too.
func TestProvidedCertificateReportsPin(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	gen := newListenRemoteCommand()
	if err := gen.ParseFlags([]string{"--tls-self-signed", "--tls-cert", certPath, "--tls-key", keyPath}); err != nil {
		t.Fatal(err)
	}
	_, want, _, err := listenTLSConfig(gen, "127.0.0.1:7575", "", NewPrinter(io.Discard, io.Discard, Options{}))
	if err != nil {
		t.Fatal(err)
	}

	load := newListenRemoteCommand()
	if err := load.ParseFlags([]string{"--tls-cert", certPath, "--tls-key", keyPath}); err != nil {
		t.Fatal(err)
	}
	_, got, _, err := listenTLSConfig(load, "127.0.0.1:7575", "", NewPrinter(io.Discard, io.Discard, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if got != want || len(got) != 64 {
		t.Fatalf("pin = %q, want %q", got, want)
	}
}

func TestSelfSignedRejectsHalfKeyPair(t *testing.T) {
	cmd := newListenRemoteCommand()
	if err := cmd.ParseFlags([]string{"--tls-self-signed", "--tls-cert", filepath.Join(t.TempDir(), "only.crt")}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := listenTLSConfig(cmd, "127.0.0.1:7575", "", NewPrinter(io.Discard, io.Discard, Options{})); err == nil {
		t.Fatal("accepted --tls-cert without --tls-key")
	}
}
