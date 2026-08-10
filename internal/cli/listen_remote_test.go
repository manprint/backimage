package cli

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/server"
	"github.com/manprint/backimage/pkg/transport"
	"github.com/spf13/cobra"
)

func TestListenRemoteRefusesUnsafeStartup(t *testing.T) {
	_, _, err := runRoot(t, "listen-remote", "--tls-self-signed", "--bind-address", "127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), "authentication is required") {
		t.Fatalf("error = %v", err)
	}
	_, _, err = runRoot(t, "listen-remote", "--auth-token", "x", "--bind-address", "127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), "TLS certificate required") {
		t.Fatalf("TLS error = %v", err)
	}
	_, _, err = runRoot(t, "listen-remote", "--tls-self-signed", "--auth-token", "x", "--max-sessions", "0")
	if err == nil || !strings.Contains(err.Error(), "max-sessions") {
		t.Fatalf("sessions error = %v", err)
	}
}

func TestListenTLSAndLimitHelpers(t *testing.T) {
	cmd := newListenRemoteCommand()
	if err := cmd.ParseFlags([]string{"--tls-self-signed", "--auth-token", "x"}); err != nil {
		t.Fatal(err)
	}
	cfg, pin, mtls, err := listenTLSConfig(cmd, "127.0.0.1:0", "", NewPrinter(io.Discard, io.Discard, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 || len(cfg.Certificates) != 1 || len(pin) != 64 || mtls {
		t.Fatalf("TLS config pin=%q mtls=%v cfg=%#v", pin, mtls, cfg)
	}
	if _, err := hex.DecodeString(pin); err != nil {
		t.Fatal(err)
	}
	for value, want := range map[string]uint64{"0": 0, "1KiB": 1024, "2MiB": 2 << 20} {
		got, err := parseLimitFlag(value)
		if err != nil || got != want {
			t.Fatalf("limit %s = %d, %v", value, got, err)
		}
	}
	if _, err := parseLimitFlag("nope"); err == nil {
		t.Fatal("invalid limit accepted")
	}
}

func TestSharedAuthTokenFileAndConflicts(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(" secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newListenRemoteCommand()
	if err := cmd.ParseFlags([]string{"--auth-token-file", tokenFile}); err != nil {
		t.Fatal(err)
	}
	if got, err := sharedAuthToken(cmd); err != nil || got != "secret" {
		t.Fatalf("token = %q, %v", got, err)
	}
	cmd = newListenRemoteCommand()
	_ = cmd.ParseFlags([]string{"--auth-token", "a", "--auth-token-file", tokenFile})
	if _, err := sharedAuthToken(cmd); err == nil {
		t.Fatal("conflicting token sources accepted")
	}
}

func TestBackupRemoteDryRunDoesNotDial(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin := strings.Repeat("00", 32)
	out, _, err := runRoot(t,
		"backup", tree, "--repo", "example.com/me/x", "--remote", "127.0.0.1:1",
		"--tls-pin", pin, "--dry-run", "--no-encrypt", "--allow-degraded", "--runnable=false",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestQUICFlagsAreHiddenAndValidated(t *testing.T) {
	for _, cmd := range []*cobra.Command{newBackupCommand(), newListenRemoteCommand()} {
		for _, name := range []string{"x-quic-streams", "x-quic-window", "x-quic-gso", "x-quic-cc"} {
			flag := cmd.Flags().Lookup(name)
			if flag == nil || !flag.Hidden {
				t.Fatalf("%s hidden flag %q = %#v", cmd.Name(), name, flag)
			}
		}
	}

	cmd := newBackupCommand()
	if err := cmd.ParseFlags([]string{"--udp", "--x-quic-window", "1048576"}); err != nil {
		t.Fatal(err)
	}
	cfg := transport.Config{}
	if err := applyQUICExperimentalFlags(cmd, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.QUICStreams != 1 || cfg.QUICWindow != 1<<20 {
		t.Fatalf("QUIC config = %#v", cfg)
	}

	cmd = newBackupCommand()
	if err := cmd.ParseFlags([]string{"--udp", "--x-quic-streams", "2"}); err != nil {
		t.Fatal(err)
	}
	if err := applyQUICExperimentalFlags(cmd, &transport.Config{}); err == nil {
		t.Fatal("multi-stream protocol v1 tuning accepted")
	}
	_, _, err := runRoot(t, "listen-remote", "--also-tcp")
	if err == nil || !strings.Contains(err.Error(), "requires --udp") {
		t.Fatalf("--also-tcp error = %v", err)
	}
}

func TestMetricsServerLifecycle(t *testing.T) {
	httpServer, err := startMetricsServer("127.0.0.1:0", new(server.Metrics), nil)
	if err != nil || httpServer == nil {
		t.Fatalf("server = %v, %v", httpServer, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if noServer, err := startMetricsServer("", new(server.Metrics), nil); err != nil || noServer != nil {
		t.Fatalf("empty metrics server = %v, %v", noServer, err)
	}
}
