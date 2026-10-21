package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/manprint/backimage/pkg/backup"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/registry"
)

// ExitCodeFor had no fallback for these: a path that forgot to wrap one by
// hand exited 1 instead of the documented code.
func TestExitCodeForClassifiesWrappedSentinels(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Kind
	}{
		{fmt.Errorf("unlock: %w", crypt.ErrWrongPassphrase), KindPassphrase},
		{fmt.Errorf("read: %w", crypt.ErrNoPassphrase), KindPassphrase},
		{fmt.Errorf("stream: %w", context.Canceled), KindInterrupted},
		{fmt.Errorf("list tags: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}), KindNetwork},
		{fmt.Errorf("x: %w", backup.ErrPrivileges), KindPermission},
		{fmt.Errorf("open: %w", fs.ErrPermission), KindPermission},
		{fmt.Errorf("push: %w", registry.ErrPublishedMismatch), KindIntegrity},
		{errors.New("anything else"), KindGeneric},
		// syscall.Errno has Timeout and Temporary, so it satisfies net.Error:
		// a missing file or a full disk is no network failure.
		{fmt.Errorf("doctor: %w", &fs.PathError{Op: "lstat", Path: "/nope", Err: syscall.ENOENT}), KindGeneric},
		{fmt.Errorf("write: %w", syscall.ENOSPC), KindGeneric},
		{fmt.Errorf("read: %w", syscall.EIO), KindGeneric},
		{fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), KindNetwork},
		{fmt.Errorf("resolve: %w", &net.DNSError{Err: "no such host", Name: "registry.invalid"}), KindNetwork},
	} {
		if got := ExitCodeFor(tc.err); got != int(tc.want) {
			t.Errorf("ExitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// The backup's own classification came first and got it wrong: any message
// mentioning "registry" or "push" was a network error, a read-back that found
// the registry holding something else included, and every *Error the
// pipeline passed through lost its Kind.
func TestBackupErrorKeepsItsClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Kind
	}{
		{fmt.Errorf("push: %w", registry.ErrPublishedMismatch), KindIntegrity},
		{New(KindUsage, "", "passphrase: empty passphrase"), KindUsage},
		{fmt.Errorf("preflight: chown: %w", backup.ErrPrivileges), KindPermission},
		{errors.New("push: 503 from the registry"), KindNetwork},
	} {
		if got := ExitCodeFor(backupError(context.Background(), tc.err)); got != int(tc.want) {
			t.Errorf("backupError(%v) exits %d, want %d", tc.err, got, tc.want)
		}
	}
}

// Observed: an unprivileged backup without --allow-degraded stopped on the
// preflight with exit 1, not the 3 the convention reserves for privileges.
func TestUnprivilegedBackupExitsThree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root passes the privilege preflight")
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runRoot(t, "backup", tree, "--repo", "example.test/team/p", "--tag", "t1", "--output", "oci-layout",
		"--output-path", filepath.Join(t.TempDir(), "layout"), "--no-encrypt", "--platform", "linux/amd64")
	if ExitCodeFor(err) != int(KindPermission) {
		t.Fatalf("unprivileged backup exit = %d (%v), want %d", ExitCodeFor(err), err, KindPermission)
	}
}

// A passphrase file that holds no passphrase is a usage mistake, exit 2: the
// pipeline returned the *Error the CLI built, and the CLI re-wrapped it as 1.
func TestBackupKeepsTheKindOfAPassphraseError(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runRoot(t, "backup", tree, "--repo", "example.test/team/p", "--tag", "t1", "--output", "oci-layout",
		"--output-path", filepath.Join(t.TempDir(), "layout"), "--allow-degraded", "--platform", "linux/amd64",
		"--passphrase-file", empty)
	if ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("empty passphrase file exit = %d (%v), want %d", ExitCodeFor(err), err, KindUsage)
	}
}
