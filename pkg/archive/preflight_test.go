//go:build unix

package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProcCapEff(t *testing.T) {
	fake := "Name:\tbackimage\nCapEff:\t0000000000040000\nCapBnd:\t0000000000000000\n"
	eff, err := parseCapEff(fake)
	if err != nil {
		t.Fatal(err)
	}
	if eff != 0x40000 {
		t.Fatalf("CapEff = %#x, want 0x40000", eff)
	}
	if _, err := parseCapEff("NoCapEffHere"); err != nil {
		t.Fatalf("missing CapEff must not be an error (non-Linux): %v", err)
	}
	if _, err := parseCapEff("CapEff:\tzzz\n"); err == nil {
		t.Fatal("malformed CapEff must error")
	}
}

func TestHasCap(t *testing.T) {
	eff := uint64(1<<capChown) | uint64(1<<capDACReadSearch)
	if !hasCap(eff, capChown) || !hasCap(eff, capDACReadSearch) {
		t.Fatal("bit checks")
	}
	if hasCap(eff, capMknod) {
		t.Fatal("capMknod must not be set")
	}
}

func TestPreflightBackupUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(locked, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(locked, 65534, 65534); err != nil {
			t.Fatalf("chown (needs root): %v", err)
		}
	}
	caps, err := PreflightBackup(context.Background(), []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	var readAll *Capability
	for i := range caps {
		if caps[i].Name == "read-all-files" {
			readAll = &caps[i]
		}
	}
	if readAll == nil {
		t.Fatal("read-all-files capability missing from report")
	}
	if readAll.Available && os.Geteuid() != 0 && !hasCap(readCapEff(), capDACReadSearch) {
		t.Fatalf("read-all-files must be unavailable for a 0000 foreign file: %+v", *readAll)
	}
	if !readAll.Available && readAll.Remedy == "" {
		t.Fatal("Remedy must never be empty when Available is false")
	}
}

func TestPreflightBackupEmptyRoots(t *testing.T) {
	if _, err := PreflightBackup(context.Background(), nil); err == nil {
		t.Fatal("no roots must error")
	}
}

func TestPreflightRestore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new-dir")
	caps, err := PreflightRestore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("PreflightRestore must create the destination: %v", err)
	}
	if len(caps) != 4 {
		t.Fatalf("PreflightRestore: want 4 capabilities, got %d", len(caps))
	}
	for i := range caps {
		if caps[i].Name == "read-all-files" && !caps[i].Available {
			t.Fatalf("fresh temp dir must be readable: %+v", caps[i])
		}
	}
	if _, err := PreflightRestore(context.Background(), ""); err == nil {
		t.Fatal("empty destination must error")
	}
}

func TestWriterStrictNoPartialTar(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(locked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: true})
	if err := w.AddRoot(context.Background(), dir); err == nil {
		t.Fatal("strict walk of unreadable file must error")
	}
	// The root dir header may legitimately precede the error; what must not
	// happen is any payload content or a completed, openable tar.
	if strings.Contains(buf.String(), "locked.txt") {
		t.Fatal("unreadable file content leaked into the archive")
	}
}
