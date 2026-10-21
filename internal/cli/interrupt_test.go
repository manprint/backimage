package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A cancelled run context is what SIGINT and SIGTERM produce. restore used to
// report the cancelled registry call as whatever error it became, never as
// the documented exit 7.
func TestExecuteReportsAnInterruptedRunAsSeven(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, "docker"))
	t.Setenv("BACKIMAGE_AUTH_FILE", filepath.Join(home, "auth.json"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(t.TempDir(), "out.tar")
	err := Execute(ctx, []string{"restore", "127.0.0.1:1/demo:v1", "--output", out})
	if got := ExitCodeFor(err); got != int(KindInterrupted) {
		t.Fatalf("interrupted restore exit = %d (%v), want %d", got, err, KindInterrupted)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("interrupted restore left %s behind: %v", out, statErr)
	}
	// A command that finishes is not turned into a failure by a signal that
	// arrived too late to stop it.
	if err := Execute(ctx, []string{"version"}); err != nil {
		t.Fatalf("version under a cancelled context = %v", err)
	}
}
